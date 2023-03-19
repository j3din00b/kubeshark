package cmd

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"time"

	"github.com/kubeshark/kubeshark/config"
	"github.com/kubeshark/kubeshark/errormessage"
	"github.com/kubeshark/kubeshark/internal/connect"
	"github.com/kubeshark/kubeshark/kubernetes"
	"github.com/kubeshark/kubeshark/misc"
	"github.com/kubeshark/kubeshark/misc/fsUtils"
	"github.com/kubeshark/kubeshark/resources"
	"github.com/rs/zerolog/log"
)

func startProxyReportErrorIfAny(kubernetesProvider *kubernetes.Provider, ctx context.Context, serviceName string, podName string, proxyPortLabel string, srcPort uint16, dstPort uint16, healthCheck string) {
	httpServer, err := kubernetes.StartProxy(kubernetesProvider, config.Config.Tap.Proxy.Host, srcPort, config.Config.SelfNamespace, serviceName)
	if err != nil {
		log.Error().
			Err(errormessage.FormatError(err)).
			Msg(fmt.Sprintf("Error occured while running K8s proxy. Try setting different port using --%s", proxyPortLabel))
		return
	}

	connector := connect.NewConnector(kubernetes.GetLocalhostOnPort(srcPort), connect.DefaultRetries, connect.DefaultTimeout)
	if err := connector.TestConnection(healthCheck); err != nil {
		log.Warn().
			Str("service", serviceName).
			Msg("Couldn't connect using proxy, stopping proxy and trying to create port-forward...")
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Error().
				Err(errormessage.FormatError(err)).
				Msg("Error occurred while stopping proxy.")
		}

		podRegex, _ := regexp.Compile(podName)
		if _, err := kubernetes.NewPortForward(kubernetesProvider, config.Config.SelfNamespace, podRegex, srcPort, dstPort, ctx); err != nil {
			log.Error().
				Str("pod-regex", podRegex.String()).
				Err(errormessage.FormatError(err)).
				Msg(fmt.Sprintf("Error occured while running port forward. Try setting different port using --%s", proxyPortLabel))
			return
		}

		connector = connect.NewConnector(kubernetes.GetLocalhostOnPort(srcPort), connect.DefaultRetries, connect.DefaultTimeout)
		if err := connector.TestConnection(healthCheck); err != nil {
			log.Error().
				Str("service", serviceName).
				Err(errormessage.FormatError(err)).
				Msg("Couldn't connect to service.")
			return
		}
	}
}

func getKubernetesProviderForCli() (*kubernetes.Provider, error) {
	kubeConfigPath := config.Config.KubeConfigPath()
	kubernetesProvider, err := kubernetes.NewProvider(kubeConfigPath, config.Config.Kube.Context)
	if err != nil {
		handleKubernetesProviderError(err)
		return nil, err
	}

	log.Info().Str("path", kubeConfigPath).Msg("Using kubeconfig:")

	if err := kubernetesProvider.ValidateNotProxy(); err != nil {
		handleKubernetesProviderError(err)
		return nil, err
	}

	kubernetesVersion, err := kubernetesProvider.GetKubernetesVersion()
	if err != nil {
		handleKubernetesProviderError(err)
		return nil, err
	}

	if err := kubernetes.ValidateKubernetesVersion(kubernetesVersion); err != nil {
		handleKubernetesProviderError(err)
		return nil, err
	}

	return kubernetesProvider, nil
}

func handleKubernetesProviderError(err error) {
	var clusterBehindProxyErr *kubernetes.ClusterBehindProxyError
	if ok := errors.As(err, &clusterBehindProxyErr); ok {
		log.Error().Msg(fmt.Sprintf("Cannot establish http-proxy connection to the Kubernetes cluster. If you’re using Lens or similar tool, please run '%s' with regular kubectl config using --%v %v=$HOME/.kube/config flag", misc.Program, config.SetCommandName, config.KubeConfigPathConfigName))
	} else {
		log.Error().Err(err).Send()
	}
}

func finishSelfExecution(kubernetesProvider *kubernetes.Provider, isNsRestrictedMode bool, selfNamespace string, withoutCleanup bool) {
	removalCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	dumpLogsIfNeeded(removalCtx, kubernetesProvider)
	if !withoutCleanup {
		resources.CleanUpSelfResources(removalCtx, cancel, kubernetesProvider, isNsRestrictedMode, selfNamespace)
	}
}

func dumpLogsIfNeeded(ctx context.Context, kubernetesProvider *kubernetes.Provider) {
	if !config.Config.DumpLogs {
		return
	}
	dotDir := misc.GetDotFolderPath()
	filePath := path.Join(dotDir, fmt.Sprintf("%s_logs_%s.zip", misc.Program, time.Now().Format("2006_01_02__15_04_05")))
	if err := fsUtils.DumpLogs(ctx, kubernetesProvider, filePath); err != nil {
		log.Error().Err(err).Msg("Failed to dump logs.")
	}
}
