package cmd

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/kubeshark/kubeshark/docker"
	"github.com/kubeshark/kubeshark/internal/connect"
	"github.com/kubeshark/kubeshark/misc"
	"github.com/kubeshark/kubeshark/resources"
	"github.com/kubeshark/kubeshark/utils"

	core "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubeshark/kubeshark/config"
	"github.com/kubeshark/kubeshark/config/configStructs"
	"github.com/kubeshark/kubeshark/errormessage"
	"github.com/kubeshark/kubeshark/kubernetes"
	"github.com/rs/zerolog/log"
)

const cleanupTimeout = time.Minute

type tapState struct {
	startTime                time.Time
	targetNamespaces         []string
	selfServiceAccountExists bool
}

var state tapState
var connector *connect.Connector
var hubPodReady bool
var frontPodReady bool
var proxyDone bool

func tap() {
	state.startTime = time.Now()
	docker.SetRegistry(config.Config.Tap.Docker.Registry)
	docker.SetTag(config.Config.Tap.Docker.Tag)
	log.Info().Str("registry", docker.GetRegistry()).Str("tag", docker.GetTag()).Msg("Using Docker:")
	if config.Config.Tap.Pcap != "" {
		pcap(config.Config.Tap.Pcap)
		return
	}

	log.Info().
		Str("limit", config.Config.Tap.StorageLimit).
		Msg(fmt.Sprintf("%s will store the traffic up to a limit (per node). Oldest TCP streams will be removed once the limit is reached.", misc.Software))

	connector = connect.NewConnector(kubernetes.GetLocalhostOnPort(config.Config.Tap.Proxy.Hub.SrcPort), connect.DefaultRetries, connect.DefaultTimeout)

	kubernetesProvider, err := getKubernetesProviderForCli()
	if err != nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // cancel will be called when this function exits

	state.targetNamespaces = getNamespaces(kubernetesProvider)

	if config.Config.IsNsRestrictedMode() {
		if len(state.targetNamespaces) != 1 || !utils.Contains(state.targetNamespaces, config.Config.SelfNamespace) {
			log.Error().Msg(fmt.Sprintf("%s can't resolve IPs in other namespaces when running in namespace restricted mode. You can use the same namespace for --%s and --%s", misc.Software, configStructs.NamespacesLabel, config.SelfNamespaceConfigName))
			return
		}
	}

	log.Info().Strs("namespaces", state.targetNamespaces).Msg("Targetting pods in:")

	if err := printTargettedPodsPreview(ctx, kubernetesProvider, state.targetNamespaces); err != nil {
		log.Error().Err(errormessage.FormatError(err)).Msg("Error listing pods!")
	}

	if config.Config.Tap.DryRun {
		return
	}

	log.Info().Msg(fmt.Sprintf("Waiting for the creation of %s resources...", misc.Software))
	if state.selfServiceAccountExists, err = resources.CreateHubResources(ctx, kubernetesProvider, config.Config.IsNsRestrictedMode(), config.Config.SelfNamespace, config.Config.Tap.Resources.Hub, config.Config.ImagePullPolicy(), config.Config.Tap.Debug); err != nil {
		var statusError *k8serrors.StatusError
		if errors.As(err, &statusError) && (statusError.ErrStatus.Reason == metav1.StatusReasonAlreadyExists) {
			log.Warn().Msg(fmt.Sprintf("%s is already running in this namespace, change the `selfnamespace` configuration or run `%s clean` to remove the currently running %s instance", misc.Software, misc.Program, misc.Software))
		} else {
			defer resources.CleanUpSelfResources(ctx, cancel, kubernetesProvider, config.Config.IsNsRestrictedMode(), config.Config.SelfNamespace)
			log.Error().Err(errormessage.FormatError(err)).Msg("Error creating resources!")
		}

		return
	}

	defer finishTapExecution(kubernetesProvider)

	go watchHubEvents(ctx, kubernetesProvider, cancel)
	go watchHubPod(ctx, kubernetesProvider, cancel)
	go watchFrontPod(ctx, kubernetesProvider, cancel)

	// block until exit signal or error
	utils.WaitForTermination(ctx, cancel)
}

func finishTapExecution(kubernetesProvider *kubernetes.Provider) {
	finishSelfExecution(kubernetesProvider, config.Config.IsNsRestrictedMode(), config.Config.SelfNamespace)
}

/*
This function is a bit problematic as it might be detached from the actual pods the Kubeshark that targets.
The alternative would be to wait for Hub to be ready and then query it for the pods it listens to, this has
the arguably worse drawback of taking a relatively very long time before the user sees which pods are targeted, if any.
*/
func printTargettedPodsPreview(ctx context.Context, kubernetesProvider *kubernetes.Provider, namespaces []string) error {
	if matchingPods, err := kubernetesProvider.ListAllRunningPodsMatchingRegex(ctx, config.Config.Tap.PodRegex(), namespaces); err != nil {
		return err
	} else {
		if len(matchingPods) == 0 {
			printNoPodsFoundSuggestion(namespaces)
		}
		for _, targettedPod := range matchingPods {
			log.Info().Msg(fmt.Sprintf("New pod: %s", fmt.Sprintf(utils.Green, targettedPod.Name)))
		}
		return nil
	}
}

func startWorkerSyncer(ctx context.Context, cancel context.CancelFunc, provider *kubernetes.Provider, targetNamespaces []string, startTime time.Time) error {
	workerSyncer, err := kubernetes.CreateAndStartWorkerSyncer(ctx, provider, kubernetes.WorkerSyncerConfig{
		TargetNamespaces:         targetNamespaces,
		PodFilterRegex:           *config.Config.Tap.PodRegex(),
		SelfNamespace:            config.Config.SelfNamespace,
		WorkerResources:          config.Config.Tap.Resources.Worker,
		ImagePullPolicy:          config.Config.ImagePullPolicy(),
		SelfServiceAccountExists: state.selfServiceAccountExists,
		ServiceMesh:              config.Config.Tap.ServiceMesh,
		Tls:                      config.Config.Tap.Tls,
		Debug:                    config.Config.Tap.Debug,
	}, startTime)

	if err != nil {
		return err
	}

	go func() {
		for {
			select {
			case syncerErr, ok := <-workerSyncer.ErrorOut:
				if !ok {
					log.Debug().Msg("workerSyncer err channel closed, ending listener loop")
					return
				}
				log.Error().Msg(getK8sTapManagerErrorText(syncerErr))
				cancel()
			case _, ok := <-workerSyncer.TapPodChangesOut:
				if !ok {
					log.Debug().Msg("workerSyncer pod changes channel closed, ending listener loop")
					return
				}
				go connector.PostTargettedPodsToHub(workerSyncer.CurrentlyTargettedPods)
			case pod, ok := <-workerSyncer.WorkerPodsChanges:
				if !ok {
					log.Debug().Msg("workerSyncer worker status changed channel closed, ending listener loop")
					return
				}
				go connector.PostWorkerPodToHub(pod)
			case <-ctx.Done():
				log.Debug().Msg("workerSyncer event listener loop exiting due to context done")
				return
			}
		}
	}()

	return nil
}

func printNoPodsFoundSuggestion(targetNamespaces []string) {
	var suggestionStr string
	if !utils.Contains(targetNamespaces, kubernetes.K8sAllNamespaces) {
		suggestionStr = ". You can also try selecting a different namespace with -n or target all namespaces with -A"
	}
	log.Warn().Msg(fmt.Sprintf("Did not find any currently running pods that match the regex argument, %s will automatically target matching pods if any are created later%s", misc.Software, suggestionStr))
}

func getK8sTapManagerErrorText(err kubernetes.K8sTapManagerError) string {
	switch err.TapManagerReason {
	case kubernetes.TapManagerPodListError:
		return fmt.Sprintf("Failed to update currently targetted pods: %v", err.OriginalError)
	case kubernetes.TapManagerPodWatchError:
		return fmt.Sprintf("Error occured in K8s pod watch: %v", err.OriginalError)
	case kubernetes.TapManagerWorkerUpdateError:
		return fmt.Sprintf("Error updating worker: %v", err.OriginalError)
	default:
		return fmt.Sprintf("Unknown error occured in K8s tap manager: %v", err.OriginalError)
	}
}

func watchHubPod(ctx context.Context, kubernetesProvider *kubernetes.Provider, cancel context.CancelFunc) {
	podExactRegex := regexp.MustCompile(fmt.Sprintf("^%s$", kubernetes.HubPodName))
	podWatchHelper := kubernetes.NewPodWatchHelper(kubernetesProvider, podExactRegex)
	eventChan, errorChan := kubernetes.FilteredWatch(ctx, podWatchHelper, []string{config.Config.SelfNamespace}, podWatchHelper)
	isPodReady := false

	timeAfter := time.After(120 * time.Second)
	for {
		select {
		case wEvent, ok := <-eventChan:
			if !ok {
				eventChan = nil
				continue
			}

			switch wEvent.Type {
			case kubernetes.EventAdded:
				log.Info().Str("pod", kubernetes.HubPodName).Msg("Added pod.")
			case kubernetes.EventDeleted:
				log.Info().Str("pod", kubernetes.HubPodName).Msg("Removed pod.")
				cancel()
				return
			case kubernetes.EventModified:
				modifiedPod, err := wEvent.ToPod()
				if err != nil {
					log.Error().Str("pod", kubernetes.HubPodName).Err(err).Msg("While watching pod.")
					cancel()
					continue
				}

				log.Debug().
					Str("pod", kubernetes.HubPodName).
					Interface("phase", modifiedPod.Status.Phase).
					Interface("containers-statuses", modifiedPod.Status.ContainerStatuses).
					Msg("Watching pod.")

				if modifiedPod.Status.Phase == core.PodRunning && !isPodReady {
					isPodReady = true
					hubPodReady = true
					postHubStarted(ctx, kubernetesProvider, cancel)
				}

				if !proxyDone && hubPodReady && frontPodReady {
					proxyDone = true
					postFrontStarted(ctx, kubernetesProvider, cancel)
				}
			case kubernetes.EventBookmark:
				break
			case kubernetes.EventError:
				break
			}
		case err, ok := <-errorChan:
			if !ok {
				errorChan = nil
				continue
			}

			log.Error().
				Str("pod", kubernetes.HubPodName).
				Str("namespace", config.Config.SelfNamespace).
				Err(err).
				Msg("Failed creating pod.")
			cancel()

		case <-timeAfter:
			if !isPodReady {
				log.Error().
					Str("pod", kubernetes.HubPodName).
					Msg("Pod was not ready in time.")
				cancel()
			}
		case <-ctx.Done():
			log.Debug().
				Str("pod", kubernetes.HubPodName).
				Msg("Watching pod, context done.")
			return
		}
	}
}

func watchFrontPod(ctx context.Context, kubernetesProvider *kubernetes.Provider, cancel context.CancelFunc) {
	podExactRegex := regexp.MustCompile(fmt.Sprintf("^%s$", kubernetes.FrontPodName))
	podWatchHelper := kubernetes.NewPodWatchHelper(kubernetesProvider, podExactRegex)
	eventChan, errorChan := kubernetes.FilteredWatch(ctx, podWatchHelper, []string{config.Config.SelfNamespace}, podWatchHelper)
	isPodReady := false

	timeAfter := time.After(120 * time.Second)
	for {
		select {
		case wEvent, ok := <-eventChan:
			if !ok {
				eventChan = nil
				continue
			}

			switch wEvent.Type {
			case kubernetes.EventAdded:
				log.Info().Str("pod", kubernetes.FrontPodName).Msg("Added pod.")
			case kubernetes.EventDeleted:
				log.Info().Str("pod", kubernetes.FrontPodName).Msg("Removed pod.")
				cancel()
				return
			case kubernetes.EventModified:
				modifiedPod, err := wEvent.ToPod()
				if err != nil {
					log.Error().Str("pod", kubernetes.FrontPodName).Err(err).Msg("While watching pod.")
					cancel()
					continue
				}

				log.Debug().
					Str("pod", kubernetes.FrontPodName).
					Interface("phase", modifiedPod.Status.Phase).
					Interface("containers-statuses", modifiedPod.Status.ContainerStatuses).
					Msg("Watching pod.")

				if modifiedPod.Status.Phase == core.PodRunning && !isPodReady {
					isPodReady = true
					frontPodReady = true
				}

				if !proxyDone && hubPodReady && frontPodReady {
					proxyDone = true
					postFrontStarted(ctx, kubernetesProvider, cancel)
				}
			case kubernetes.EventBookmark:
				break
			case kubernetes.EventError:
				break
			}
		case err, ok := <-errorChan:
			if !ok {
				errorChan = nil
				continue
			}

			log.Error().
				Str("pod", kubernetes.FrontPodName).
				Str("namespace", config.Config.SelfNamespace).
				Err(err).
				Msg("Failed creating pod.")
			cancel()

		case <-timeAfter:
			if !isPodReady {
				log.Error().
					Str("pod", kubernetes.FrontPodName).
					Msg("Pod was not ready in time.")
				cancel()
			}
		case <-ctx.Done():
			log.Debug().
				Str("pod", kubernetes.FrontPodName).
				Msg("Watching pod, context done.")
			return
		}
	}
}

func watchHubEvents(ctx context.Context, kubernetesProvider *kubernetes.Provider, cancel context.CancelFunc) {
	podExactRegex := regexp.MustCompile(fmt.Sprintf("^%s", kubernetes.HubPodName))
	eventWatchHelper := kubernetes.NewEventWatchHelper(kubernetesProvider, podExactRegex, "pod")
	eventChan, errorChan := kubernetes.FilteredWatch(ctx, eventWatchHelper, []string{config.Config.SelfNamespace}, eventWatchHelper)
	for {
		select {
		case wEvent, ok := <-eventChan:
			if !ok {
				eventChan = nil
				continue
			}

			event, err := wEvent.ToEvent()
			if err != nil {
				log.Error().
					Str("pod", kubernetes.HubPodName).
					Err(err).
					Msg("Parsing resource event.")
				continue
			}

			if state.startTime.After(event.CreationTimestamp.Time) {
				continue
			}

			log.Debug().
				Str("pod", kubernetes.HubPodName).
				Str("event", event.Name).
				Time("time", event.CreationTimestamp.Time).
				Str("name", event.Regarding.Name).
				Str("kind", event.Regarding.Kind).
				Str("reason", event.Reason).
				Str("note", event.Note).
				Msg("Watching events.")

			switch event.Reason {
			case "FailedScheduling", "Failed":
				log.Error().
					Str("pod", kubernetes.HubPodName).
					Str("event", event.Name).
					Time("time", event.CreationTimestamp.Time).
					Str("name", event.Regarding.Name).
					Str("kind", event.Regarding.Kind).
					Str("reason", event.Reason).
					Str("note", event.Note).
					Msg("Watching events.")
				cancel()

			}
		case err, ok := <-errorChan:
			if !ok {
				errorChan = nil
				continue
			}

			log.Error().
				Str("pod", kubernetes.HubPodName).
				Err(err).
				Msg("While watching events.")

		case <-ctx.Done():
			log.Debug().
				Str("pod", kubernetes.HubPodName).
				Msg("Watching pod events, context done.")
			return
		}
	}
}

func postHubStarted(ctx context.Context, kubernetesProvider *kubernetes.Provider, cancel context.CancelFunc) {
	startProxyReportErrorIfAny(kubernetesProvider, ctx, cancel, kubernetes.HubServiceName, configStructs.ProxyFrontPortLabel, config.Config.Tap.Proxy.Hub.SrcPort, config.Config.Tap.Proxy.Hub.DstPort, "/echo")

	if err := startWorkerSyncer(ctx, cancel, kubernetesProvider, state.targetNamespaces, state.startTime); err != nil {
		log.Error().Err(errormessage.FormatError(err)).Msg("Error starting worker syncer")
		cancel()
	}

	url := kubernetes.GetLocalhostOnPort(config.Config.Tap.Proxy.Hub.SrcPort)
	log.Info().Str("url", url).Msg(fmt.Sprintf(utils.Green, "Hub is available at:"))
}

func postFrontStarted(ctx context.Context, kubernetesProvider *kubernetes.Provider, cancel context.CancelFunc) {
	startProxyReportErrorIfAny(kubernetesProvider, ctx, cancel, kubernetes.FrontServiceName, configStructs.ProxyHubPortLabel, config.Config.Tap.Proxy.Front.SrcPort, config.Config.Tap.Proxy.Front.DstPort, "")

	url := kubernetes.GetLocalhostOnPort(config.Config.Tap.Proxy.Front.SrcPort)
	log.Info().Str("url", url).Msg(fmt.Sprintf(utils.Green, fmt.Sprintf("%s is available at:", misc.Software)))

	if !config.Config.HeadlessMode {
		utils.OpenBrowser(url)
	}
}

func getNamespaces(kubernetesProvider *kubernetes.Provider) []string {
	if config.Config.Tap.AllNamespaces {
		return []string{kubernetes.K8sAllNamespaces}
	} else if len(config.Config.Tap.Namespaces) > 0 {
		return utils.Unique(config.Config.Tap.Namespaces)
	} else {
		currentNamespace, err := kubernetesProvider.CurrentNamespace()
		if err != nil {
			log.Fatal().Err(err).Msg("Error getting current namespace!")
		}
		return []string{currentNamespace}
	}
}
