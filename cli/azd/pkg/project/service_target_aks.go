package project

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v2"
	"github.com/azure/azure-dev/cli/azd/pkg/async"
	"github.com/azure/azure-dev/cli/azd/pkg/azure"
	"github.com/azure/azure-dev/cli/azd/pkg/environment"
	"github.com/azure/azure-dev/cli/azd/pkg/ext"
	"github.com/azure/azure-dev/cli/azd/pkg/input"
	"github.com/azure/azure-dev/cli/azd/pkg/output"
	"github.com/azure/azure-dev/cli/azd/pkg/tools"
	"github.com/azure/azure-dev/cli/azd/pkg/tools/azcli"
	"github.com/azure/azure-dev/cli/azd/pkg/tools/kubectl"
)

const (
	defaultDeploymentPath = "manifests"
)

// The AKS configuration options
type AksOptions struct {
	// The namespace used for deploying k8s resources. Defaults to the project name
	Namespace string `yaml:"namespace"`
	// The relative folder path from the service that contains the k8s deployment manifests. Defaults to 'manifests'
	DeploymentPath string `yaml:"deploymentPath"`
	// The services ingress configuration options
	Ingress AksIngressOptions `yaml:"ingress"`
	// The services deployment configuration options
	Deployment AksDeploymentOptions `yaml:"deployment"`
	// The services service configuration options
	Service AksServiceOptions `yaml:"service"`
}

// The AKS ingress options
type AksIngressOptions struct {
	Name         string `yaml:"name"`
	RelativePath string `yaml:"relativePath"`
}

// The AKS deployment options
type AksDeploymentOptions struct {
	Name string `yaml:"name"`
}

// The AKS service configuration options
type AksServiceOptions struct {
	Name string `yaml:"name"`
}

type aksTarget struct {
	env                    *environment.Environment
	envManager             environment.Manager
	console                input.Console
	managedClustersService azcli.ManagedClustersService
	resourceManager        ResourceManager
	kubectl                kubectl.KubectlCli
	containerHelper        *ContainerHelper
}

// Creates a new instance of the AKS service target
func NewAksTarget(
	env *environment.Environment,
	envManager environment.Manager,
	console input.Console,
	managedClustersService azcli.ManagedClustersService,
	resourceManager ResourceManager,
	kubectlCli kubectl.KubectlCli,
	containerHelper *ContainerHelper,
) ServiceTarget {
	return &aksTarget{
		env:                    env,
		envManager:             envManager,
		console:                console,
		managedClustersService: managedClustersService,
		resourceManager:        resourceManager,
		kubectl:                kubectlCli,
		containerHelper:        containerHelper,
	}
}

// Gets the required external tools to support the AKS service
func (t *aksTarget) RequiredExternalTools(ctx context.Context) []tools.ExternalTool {
	allTools := []tools.ExternalTool{}
	allTools = append(allTools, t.containerHelper.RequiredExternalTools(ctx)...)
	allTools = append(allTools, t.kubectl)

	return allTools
}

// Initializes the AKS service target
func (t *aksTarget) Initialize(ctx context.Context, serviceConfig *ServiceConfig) error {
	// Ensure that the k8s context has been configured by the time a deploy operation is performed.
	// We attach to "postprovision" so that any predeploy or postprovision hooks can take advantage of the configuration
	err := serviceConfig.Project.AddHandler(
		"postprovision",
		func(ctx context.Context, args ProjectLifecycleEventArgs) error {
			return t.setK8sContext(ctx, serviceConfig, "postprovision")
		},
	)

	if err != nil {
		return fmt.Errorf("failed adding postprovision handler, %w", err)
	}

	// Ensure that the k8s context has been configured by the time a deploy operation is performed.
	// We attach to "predeploy" so that any predeploy hooks can take advantage of the configuration
	err = serviceConfig.AddHandler("predeploy", func(ctx context.Context, args ServiceLifecycleEventArgs) error {
		return t.setK8sContext(ctx, serviceConfig, "predeploy")
	})

	if err != nil {
		return fmt.Errorf("failed adding predeploy handler, %w", err)
	}

	return nil
}

// Prepares and tags the container image from the build output based on the specified service configuration
func (t *aksTarget) Package(
	ctx context.Context,
	serviceConfig *ServiceConfig,
	packageOutput *ServicePackageResult,
) *async.TaskWithProgress[*ServicePackageResult, ServiceProgress] {
	return async.RunTaskWithProgress(
		func(task *async.TaskContextWithProgress[*ServicePackageResult, ServiceProgress]) {
			task.SetResult(packageOutput)
		},
	)
}

// Deploys service container images to ACR and AKS resources to the AKS cluster
func (t *aksTarget) Deploy(
	ctx context.Context,
	serviceConfig *ServiceConfig,
	packageOutput *ServicePackageResult,
	targetResource *environment.TargetResource,
) *async.TaskWithProgress[*ServiceDeployResult, ServiceProgress] {
	return async.RunTaskWithProgress(
		func(task *async.TaskContextWithProgress[*ServiceDeployResult, ServiceProgress]) {
			if err := t.validateTargetResource(ctx, serviceConfig, targetResource); err != nil {
				task.SetError(fmt.Errorf("validating target resource: %w", err))
				return
			}

			if packageOutput == nil {
				task.SetError(errors.New("missing package output"))
				return
			}

			// Login, tag & push container image to ACR
			containerDeployTask := t.containerHelper.Deploy(ctx, serviceConfig, packageOutput, targetResource)
			syncProgress(task, containerDeployTask.Progress())

			// Sync environment
			t.kubectl.SetEnv(t.env.Dotenv())

			task.SetProgress(NewServiceProgress("Applying k8s manifests"))
			deploymentPath := serviceConfig.K8s.DeploymentPath
			if deploymentPath == "" {
				deploymentPath = defaultDeploymentPath
			}

			err := t.kubectl.Apply(
				ctx,
				filepath.Join(serviceConfig.RelativePath, deploymentPath),
				nil,
			)
			if err != nil {
				task.SetError(fmt.Errorf("failed applying kube manifests: %w", err))
				return
			}

			deploymentName := serviceConfig.K8s.Deployment.Name
			if deploymentName == "" {
				deploymentName = serviceConfig.Name
			}

			// It is not a requirement for a AZD deploy to contain a deployment object
			// If we don't find any deployment within the namespace we will continue
			task.SetProgress(NewServiceProgress("Verifying deployment"))
			deployment, err := t.waitForDeployment(ctx, deploymentName)
			if err != nil && !errors.Is(err, kubectl.ErrResourceNotFound) {
				task.SetError(err)
				return
			}

			task.SetProgress(NewServiceProgress("Fetching endpoints for AKS service"))
			endpoints, err := t.Endpoints(ctx, serviceConfig, targetResource)
			if err != nil {
				task.SetError(err)
				return
			}

			if len(endpoints) > 0 {
				// The AKS endpoints contain some additional identifying information
				// Split on common to pull out the URL as the first segment
				// The last endpoint in the array will be the most publicly exposed
				endpointParts := strings.Split(endpoints[len(endpoints)-1], ",")
				t.env.SetServiceProperty(serviceConfig.Name, "ENDPOINT_URL", endpointParts[0])
				if err := t.envManager.Save(ctx, t.env); err != nil {
					task.SetError(fmt.Errorf("failed updating environment with endpoint url, %w", err))
					return
				}
			}

			task.SetResult(&ServiceDeployResult{
				Package: packageOutput,
				TargetResourceId: azure.KubernetesServiceRID(
					targetResource.SubscriptionId(),
					targetResource.ResourceGroupName(),
					targetResource.ResourceName(),
				),
				Kind:      AksTarget,
				Details:   deployment,
				Endpoints: endpoints,
			})
		})
}

// Gets the service endpoints for the AKS service target
func (t *aksTarget) Endpoints(
	ctx context.Context,
	serviceConfig *ServiceConfig,
	targetResource *environment.TargetResource,
) ([]string, error) {
	serviceName := serviceConfig.K8s.Service.Name
	if serviceName == "" {
		serviceName = serviceConfig.Name
	}

	ingressName := serviceConfig.K8s.Service.Name
	if ingressName == "" {
		ingressName = serviceConfig.Name
	}

	// Find endpoints for any matching services
	// These endpoints would typically be internal cluster accessible endpoints
	serviceEndpoints, err := t.getServiceEndpoints(ctx, serviceConfig, serviceName)
	if err != nil && !errors.Is(err, kubectl.ErrResourceNotFound) {
		return nil, fmt.Errorf("failed retrieving service endpoints, %w", err)
	}

	// Find endpoints for any matching ingress controllers
	// These endpoints would typically be publicly accessible endpoints
	ingressEndpoints, err := t.getIngressEndpoints(ctx, serviceConfig, ingressName)
	if err != nil && !errors.Is(err, kubectl.ErrResourceNotFound) {
		return nil, fmt.Errorf("failed retrieving ingress endpoints, %w", err)
	}

	endpoints := append(serviceEndpoints, ingressEndpoints...)

	return endpoints, nil
}

func (t *aksTarget) validateTargetResource(
	ctx context.Context,
	serviceConfig *ServiceConfig,
	targetResource *environment.TargetResource,
) error {
	if targetResource.ResourceGroupName() == "" {
		return fmt.Errorf("missing resource group name: %s", targetResource.ResourceGroupName())
	}

	return nil
}

func (t *aksTarget) ensureClusterContext(
	ctx context.Context,
	serviceConfig *ServiceConfig,
	targetResource *environment.TargetResource,
	defaultNamespace string,
) (string, error) {
	kubeConfigPath := t.env.Getenv(kubectl.KubeConfigEnvVarName)
	if kubeConfigPath != "" {
		return kubeConfigPath, nil
	}

	// Login to AKS cluster
	clusterName, has := t.env.LookupEnv(environment.AksClusterEnvVarName)
	if !has {
		return "", fmt.Errorf(
			"could not determine AKS cluster, ensure %s is set as an output of your infrastructure",
			environment.AksClusterEnvVarName,
		)
	}

	log.Printf("getting AKS credentials for cluster '%s'\n", clusterName)
	clusterCreds, err := t.managedClustersService.GetUserCredentials(
		ctx,
		targetResource.SubscriptionId(),
		targetResource.ResourceGroupName(),
		clusterName,
	)
	if err != nil {
		return "", fmt.Errorf(
			"failed retrieving cluster admin credentials. Ensure your cluster has been configured to support admin credentials, %w",
			err,
		)
	}

	if len(clusterCreds.Kubeconfigs) == 0 {
		return "", fmt.Errorf(
			"cluster credentials is empty. Ensure your cluster has been configured to support admin credentials. , %w",
			err,
		)
	}

	// The kubeConfig that we care about will also be at position 0
	// I don't know if there is a valid use case where this credential results would container multiple configs
	kubeConfigPath, err = t.configureK8sContext(ctx, clusterName, defaultNamespace, clusterCreds.Kubeconfigs[0])
	if err != nil {
		return "", err
	}

	return kubeConfigPath, nil
}

// Ensures the k8s namespace exists otherwise creates it
func (t *aksTarget) ensureNamespace(ctx context.Context, namespace string) error {
	namespaceResult, err := t.kubectl.CreateNamespace(
		ctx,
		namespace,
		&kubectl.KubeCliFlags{
			DryRun: kubectl.DryRunTypeClient,
			Output: kubectl.OutputTypeYaml,
		},
	)
	if err != nil {
		return fmt.Errorf("failed creating kube namespace: %w", err)
	}

	_, err = t.kubectl.ApplyWithStdIn(ctx, namespaceResult.Stdout, nil)
	if err != nil {
		return fmt.Errorf("failed applying kube namespace: %w", err)
	}

	return nil
}

func (t *aksTarget) configureK8sContext(
	ctx context.Context,
	clusterName string,
	namespace string,
	credentialResult *armcontainerservice.CredentialResult,
) (string, error) {
	kubeConfigManager, err := kubectl.NewKubeConfigManager(t.kubectl)
	if err != nil {
		return "", err
	}

	kubeConfig, err := kubectl.ParseKubeConfig(ctx, credentialResult.Value)
	if err != nil {
		return "", fmt.Errorf(
			"failed parsing kube config. Ensure your configuration is valid yaml. %w",
			err,
		)
	}

	// Set default namespace for the context
	// This avoids having to specify the namespace for every kubectl command
	kubeConfig.Contexts[0].Context.Namespace = namespace
	kubeConfigPath, err := kubeConfigManager.AddOrUpdateContext(ctx, clusterName, kubeConfig)
	if err != nil {
		return "", fmt.Errorf("failed adding/updating kube context, %w", err)
	}

	if _, err := t.kubectl.ConfigUseContext(ctx, clusterName, nil); err != nil {
		return "", fmt.Errorf(
			"failed setting kube context '%s'. Ensure the specified context exists. %w", clusterName,
			err,
		)
	}

	return kubeConfigPath, nil
}

// Finds a deployment using the specified deploymentNameFilter string
// Waits until the deployment rollout is complete and all replicas are accessible
// Additionally confirms rollout is complete by checking the rollout status
func (t *aksTarget) waitForDeployment(
	ctx context.Context,
	deploymentNameFilter string,
) (*kubectl.Deployment, error) {
	// The deployment can appear like it has succeeded when a previous deployment
	// was already in place.
	deployment, err := kubectl.WaitForResource(
		ctx, t.kubectl, kubectl.ResourceTypeDeployment,
		func(deployment *kubectl.Deployment) bool {
			return strings.Contains(deployment.Metadata.Name, deploymentNameFilter)
		},
		func(deployment *kubectl.Deployment) bool {
			return deployment.Status.AvailableReplicas == deployment.Spec.Replicas
		},
	)

	if err != nil {
		return nil, err
	}

	// Check the rollout status
	// This can be a long operation when the deployment is in a failed state such as an ImagePullBackOff loop
	_, err = t.kubectl.RolloutStatus(ctx, deployment.Metadata.Name, nil)
	if err != nil {
		return nil, err
	}

	return deployment, nil
}

// Finds an ingress using the specified ingressNameFilter string
// Waits until the ingress LoadBalancer has assigned a valid IP address
func (t *aksTarget) waitForIngress(
	ctx context.Context,
	ingressNameFilter string,
) (*kubectl.Ingress, error) {
	return kubectl.WaitForResource(
		ctx, t.kubectl, kubectl.ResourceTypeIngress,
		func(ingress *kubectl.Ingress) bool {
			return strings.Contains(ingress.Metadata.Name, ingressNameFilter)
		},
		func(ingress *kubectl.Ingress) bool {
			for _, config := range ingress.Status.LoadBalancer.Ingress {
				if config.Ip != "" {
					return true
				}
			}

			return false
		},
	)
}

// Finds a service using the specified serviceNameFilter string
// Waits until the service is available
func (t *aksTarget) waitForService(
	ctx context.Context,
	serviceNameFilter string,
) (*kubectl.Service, error) {
	return kubectl.WaitForResource(
		ctx, t.kubectl, kubectl.ResourceTypeService,
		func(service *kubectl.Service) bool {
			return strings.Contains(service.Metadata.Name, serviceNameFilter)
		},
		func(service *kubectl.Service) bool {
			// If the service is not a load balancer it should be immediately available
			if service.Spec.Type != kubectl.ServiceTypeLoadBalancer {
				return true
			}

			// Load balancer can take some time to be provision by AKS
			var ipAddress string
			for _, config := range service.Status.LoadBalancer.Ingress {
				if config.Ip != "" {
					ipAddress = config.Ip
					break
				}
			}

			return ipAddress != ""
		},
	)
}

// Retrieve any service endpoints for the specified serviceNameFilter
// Supports service types for LoadBalancer and ClusterIP
func (t *aksTarget) getServiceEndpoints(
	ctx context.Context,
	serviceConfig *ServiceConfig,
	serviceNameFilter string,
) ([]string, error) {
	service, err := t.waitForService(ctx, serviceNameFilter)
	if err != nil {
		return nil, err
	}

	var endpoints []string
	if service.Spec.Type == kubectl.ServiceTypeLoadBalancer {
		for _, resource := range service.Status.LoadBalancer.Ingress {
			endpoints = append(endpoints, fmt.Sprintf("http://%s, (Service, Type: LoadBalancer)", resource.Ip))
		}
	} else if service.Spec.Type == kubectl.ServiceTypeClusterIp {
		for index, ip := range service.Spec.ClusterIps {
			endpoints = append(endpoints, fmt.Sprintf("http://%s:%d, (Service, Type: ClusterIP)", ip, service.Spec.Ports[index].Port))
		}
	}

	return endpoints, nil
}

// Retrieve any ingress endpoints for the specified serviceNameFilter
// Supports service types for LoadBalancer, supports Hosts and/or IP address
func (t *aksTarget) getIngressEndpoints(
	ctx context.Context,
	serviceConfig *ServiceConfig,
	resourceFilter string,
) ([]string, error) {
	ingress, err := t.waitForIngress(ctx, resourceFilter)
	if err != nil {
		return nil, err
	}

	var endpoints []string
	var protocol string
	if len(ingress.Spec.Tls) == 0 {
		protocol = "http"
	} else {
		protocol = "https"
	}

	for index, resource := range ingress.Status.LoadBalancer.Ingress {
		var baseUrl string
		if ingress.Spec.Rules[index].Host == nil {
			baseUrl = fmt.Sprintf("%s://%s", protocol, resource.Ip)
		} else {
			baseUrl = fmt.Sprintf("%s://%s", protocol, *ingress.Spec.Rules[index].Host)
		}

		endpointUrl, err := url.JoinPath(baseUrl, serviceConfig.K8s.Ingress.RelativePath)
		if err != nil {
			return nil, fmt.Errorf("failed constructing service endpoints, %w", err)
		}

		endpoints = append(endpoints, fmt.Sprintf("%s, (Ingress, Type: LoadBalancer)", endpointUrl))
	}

	return endpoints, nil
}

func (t *aksTarget) getK8sNamespace(serviceConfig *ServiceConfig) string {
	namespace := serviceConfig.K8s.Namespace
	if namespace == "" {
		namespace = serviceConfig.Project.Name
	}

	return namespace
}

func (t *aksTarget) setK8sContext(ctx context.Context, serviceConfig *ServiceConfig, eventName ext.Event) error {
	t.kubectl.SetEnv(t.env.Dotenv())
	hasCustomKubeConfig := false

	// If a KUBECONFIG env var is set, use it.
	kubeConfigPath := t.env.Getenv(kubectl.KubeConfigEnvVarName)
	if kubeConfigPath != "" {
		t.kubectl.SetKubeConfig(kubeConfigPath)
		hasCustomKubeConfig = true
	}

	targetResource, err := t.resourceManager.GetTargetResource(ctx, t.env.GetSubscriptionId(), serviceConfig)
	if err != nil {
		return err
	}

	defaultNamespace := t.getK8sNamespace(serviceConfig)
	_, err = t.ensureClusterContext(ctx, serviceConfig, targetResource, defaultNamespace)
	if err != nil {
		return err
	}

	err = t.ensureNamespace(ctx, defaultNamespace)
	if err != nil {
		return err
	}

	// Display message to the user when we detect they are using a non-default KUBECONFIG configuration
	// In standard AZD AKS deployment users should not typically need to set a custom KUBECONFIG
	if hasCustomKubeConfig && eventName == "predeploy" {
		t.console.Message(ctx, output.WithWarningFormat("Using KUBECONFIG @ %s\n", kubeConfigPath))
	}

	return nil
}
