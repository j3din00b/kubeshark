package resources

import (
	"context"
	"fmt"

	"github.com/kubeshark/kubeshark/config"
	"github.com/kubeshark/kubeshark/docker"
	"github.com/kubeshark/kubeshark/errormessage"
	"github.com/kubeshark/kubeshark/kubernetes"
	"github.com/kubeshark/kubeshark/misc"
	"github.com/rs/zerolog/log"
	core "k8s.io/api/core/v1"
)

func CreateHubResources(ctx context.Context, kubernetesProvider *kubernetes.Provider, isNsRestrictedMode bool, selfNamespace string, hubResources kubernetes.Resources, imagePullPolicy core.PullPolicy, debug bool) (bool, error) {
	if !isNsRestrictedMode {
		if err := createSelfNamespace(ctx, kubernetesProvider, selfNamespace); err != nil {
			return false, err
		}
	}

	selfServiceAccountExists, err := createRBACIfNecessary(ctx, kubernetesProvider, isNsRestrictedMode, selfNamespace, []string{"pods", "services", "endpoints"})
	if err != nil {
		log.Warn().Err(errormessage.FormatError(err)).Msg(fmt.Sprintf("Failed to ensure the resources required for IP resolving. %s will not resolve target IPs to names.", misc.Software))
	}

	var serviceAccountName string
	if selfServiceAccountExists {
		serviceAccountName = kubernetes.ServiceAccountName
	} else {
		serviceAccountName = ""
	}

	opts := &kubernetes.PodOptions{
		Namespace:          selfNamespace,
		PodName:            kubernetes.HubPodName,
		PodImage:           docker.GetHubImage(),
		ServiceAccountName: serviceAccountName,
		Resources:          hubResources,
		ImagePullPolicy:    imagePullPolicy,
		Debug:              debug,
	}

	frontOpts := &kubernetes.PodOptions{
		Namespace:          selfNamespace,
		PodName:            kubernetes.FrontPodName,
		PodImage:           docker.GetWorkerImage(),
		ServiceAccountName: serviceAccountName,
		Resources:          hubResources,
		ImagePullPolicy:    imagePullPolicy,
		Debug:              debug,
	}

	if err := createSelfHubPod(ctx, kubernetesProvider, opts); err != nil {
		return selfServiceAccountExists, err
	}

	if err := createFrontPod(ctx, kubernetesProvider, frontOpts); err != nil {
		return selfServiceAccountExists, err
	}

	// TODO: Why the port values need to be 80?
	_, err = kubernetesProvider.CreateService(ctx, selfNamespace, kubernetes.HubServiceName, kubernetes.HubServiceName, 80, 80)
	if err != nil {
		return selfServiceAccountExists, err
	}

	log.Info().Str("service", kubernetes.HubServiceName).Msg("Successfully created a service.")

	_, err = kubernetesProvider.CreateService(ctx, selfNamespace, kubernetes.FrontServiceName, kubernetes.FrontServiceName, 80, int32(config.Config.Tap.Proxy.Front.DstPort))
	if err != nil {
		return selfServiceAccountExists, err
	}

	log.Info().Str("service", kubernetes.FrontServiceName).Msg("Successfully created a service.")

	return selfServiceAccountExists, nil
}

func createSelfNamespace(ctx context.Context, kubernetesProvider *kubernetes.Provider, selfNamespace string) error {
	_, err := kubernetesProvider.CreateNamespace(ctx, selfNamespace)
	return err
}

func createRBACIfNecessary(ctx context.Context, kubernetesProvider *kubernetes.Provider, isNsRestrictedMode bool, selfNamespace string, resources []string) (bool, error) {
	if !isNsRestrictedMode {
		if err := kubernetesProvider.CreateSelfRBAC(ctx, selfNamespace, kubernetes.ServiceAccountName, kubernetes.ClusterRoleName, kubernetes.ClusterRoleBindingName, misc.RBACVersion, resources); err != nil {
			return false, err
		}
	} else {
		if err := kubernetesProvider.CreateSelfRBACNamespaceRestricted(ctx, selfNamespace, kubernetes.ServiceAccountName, kubernetes.RoleName, kubernetes.RoleBindingName, misc.RBACVersion); err != nil {
			return false, err
		}
	}

	return true, nil
}

func createSelfHubPod(ctx context.Context, kubernetesProvider *kubernetes.Provider, opts *kubernetes.PodOptions) error {
	pod, err := kubernetesProvider.BuildHubPod(opts)
	if err != nil {
		return err
	}
	if _, err = kubernetesProvider.CreatePod(ctx, opts.Namespace, pod); err != nil {
		return err
	}
	log.Info().Str("pod", pod.Name).Msg("Successfully created a pod.")
	return nil
}

func createFrontPod(ctx context.Context, kubernetesProvider *kubernetes.Provider, opts *kubernetes.PodOptions) error {
	pod, err := kubernetesProvider.BuildFrontPod(opts, config.Config.Tap.Proxy.Host, fmt.Sprintf("%d", config.Config.Tap.Proxy.Hub.SrcPort))
	if err != nil {
		return err
	}
	if _, err = kubernetesProvider.CreatePod(ctx, opts.Namespace, pod); err != nil {
		return err
	}
	log.Info().Str("pod", pod.Name).Msg("Successfully created a pod.")
	return nil
}
