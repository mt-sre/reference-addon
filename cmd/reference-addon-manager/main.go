package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	refapis "github.com/mt-sre/reference-addon/apis"
	"github.com/mt-sre/reference-addon/internal/controllers"
	"github.com/mt-sre/reference-addon/internal/utils"
	addonsv1apis "github.com/openshift/addon-operator/apis"
	addonsv1alpha1 "github.com/openshift/addon-operator/apis/addons/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = refapis.AddToScheme(scheme)
	_ = addonsv1apis.AddToScheme(scheme)
}

func main() {
	var (
		metricsAddr          string
		pprofAddr            string
		enableLeaderElection bool
	)
	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&pprofAddr, "pprof-addr", "", "The address the pprof web endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                     scheme,
		MetricsBindAddress:         metricsAddr,
		Port:                       9443,
		LeaderElectionResourceLock: "leases",
		LeaderElection:             enableLeaderElection,
		LeaderElectionID:           "8a4hp84a6s.addon-operator-lock",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// -----
	// PPROF
	// -----
	if len(pprofAddr) > 0 {
		mux := http.NewServeMux()
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

		s := &http.Server{Addr: pprofAddr, Handler: mux}
		err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			errCh := make(chan error)
			defer func() {
				for range errCh {
				} // drain errCh for GC
			}()
			go func() {
				defer close(errCh)
				errCh <- s.ListenAndServe()
			}()

			select {
			case err := <-errCh:
				return err
			case <-ctx.Done():
				s.Close()
				return nil
			}
		}))
		if err != nil {
			setupLog.Error(err, "unable to create pprof server")
			os.Exit(1)
		}
	}

	// couple the heartbeat reporter with the manager

	// TODO(ykukreja): heartbeatCommunicatorCh to be buffered channel instead, for better congestion control?
	// already some congestion control happening vi the timeout defined under utils.CommunicateHeartbeat(...)
	heartbeatCommunicatorCh := make(chan metav1.Condition)
	configurationWatcherCh := make(chan bool)
	err = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		// no significance of having heartbeatCommunicatorCh open if this heartbeat reporter function is exited
		defer close(heartbeatCommunicatorCh)

		//TODO(ykukreja): Should the close(configurationWatcherCh) be deferred as well?

		addonName := "reference-addon"
		// initialized with a healthy heartbeat condition corresponding to Reference Addon
		currentHeartbeatCondition := metav1.Condition{
			Type:    "addons.managed.openshift.io/Healthy",
			Status:  "True",
			Reason:  "AllComponentsUp",
			Message: "Everything under reference-addon is working perfectly fine",
		}

		currentAddonInstanceConfiguration, err := utils.GetAddonInstanceConfiguration(ctx, mgr.GetClient(), addonName)
		if err != nil {
			return fmt.Errorf("failed to get the AddonInstance configuration corresponding to the Addon '%s': %w", addonName, err)
		}

		// Heartbeat reporter section: report a heartbeat at an interval ('currentAddonInstanceConfiguration.HeartbeatUpdatePeriod' seconds)
		for {
			select {
			case latestHeartbeatCondition := <-heartbeatCommunicatorCh:
				currentHeartbeatCondition = latestHeartbeatCondition
				if err := utils.SetAddonInstanceCondition(ctx, mgr.GetClient(), currentHeartbeatCondition, addonName); err != nil {
					mgr.GetLogger().Error(err, "error occurred while setting the condition", fmt.Sprintf("%+v", currentHeartbeatCondition))
				} // coz 'fallthrough' isn't allowed under select-case :'(
			case <-ctx.Done():
				return nil
			default:
				if err := utils.SetAddonInstanceCondition(ctx, mgr.GetClient(), currentHeartbeatCondition, addonName); err != nil {
					mgr.GetLogger().Error(err, "error occurred while setting the condition", fmt.Sprintf("%+v", currentHeartbeatCondition))
				}
			}

			// Watch for any configurational changes in the AddonInstance corresponding to reference-addon
			select {
			case <-configurationWatcherCh:
				currentAddonInstanceConfiguration, err = utils.GetAddonInstanceConfiguration(ctx, mgr.GetClient(), addonName)
				if err != nil {
					return fmt.Errorf("failed to get the AddonInstance configuration corresponding to the Addon '%s': %w", addonName, err)
				}
				// the following function can be absolutely anything depending how reference-addon would want to deal with AddonInstance's configuration change
				handleConfigurationChanges := func(addonsv1alpha1.AddonInstanceSpec) {}
				handleConfigurationChanges(currentAddonInstanceConfiguration)
			default:
				// do nothing, just continue if no configuration change is observed
			}
			<-time.After(currentAddonInstanceConfiguration.HeartbeatUpdatePeriod.Duration)
			fmt.Printf("\nDone waiting for: %+v", currentAddonInstanceConfiguration.HeartbeatUpdatePeriod.Duration)
		}
	}))
	if err != nil {
		setupLog.Error(err, "unable to setup heartbeat reporter")
		os.Exit(1)
	}

	if err = (&controllers.ReferenceAddonReconciler{
		Client:                       mgr.GetClient(),
		Log:                          ctrl.Log.WithName("controllers").WithName("ReferenceAddon"),
		Scheme:                       mgr.GetScheme(),
		HeartbeatCommunicatorChannel: heartbeatCommunicatorCh,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ReferenceAddon")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
