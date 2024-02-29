/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package app implements a Server object for running the scheduler.
package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	goruntime "runtime"

	"github.com/spf13/cobra"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	genericapifilters "k8s.io/apiserver/pkg/endpoints/filters"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/server"
	genericfilters "k8s.io/apiserver/pkg/server/filters"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/mux"
	"k8s.io/apiserver/pkg/server/routes"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/tools/leaderelection"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/component-base/configz"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/term"
	"k8s.io/component-base/version"
	"k8s.io/component-base/version/verflag"
	"k8s.io/klog/v2"

	schedulerserverconfig "k8s.io/kubernetes/cmd/kube-scheduler/app/config"
	"k8s.io/kubernetes/cmd/kube-scheduler/app/options"
	"k8s.io/kubernetes/pkg/scheduler"
	kubeschedulerconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/latest"
	"k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/metrics/resources"
	"k8s.io/kubernetes/pkg/scheduler/profile"
)

func init() {
	utilruntime.Must(logsapi.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
}

// Option configures a framework.Registry.
type Option func(runtime.Registry) error

// NewSchedulerCommand creates a *cobra.Command object with default parameters and registryOptions
func NewSchedulerCommand(registryOptions ...Option) *cobra.Command {
	opts := options.NewOptions()

	cmd := &cobra.Command{
		Use: "kube-scheduler",
		Long: `The Kubernetes scheduler is a control plane process which assigns
Pods to Nodes. The scheduler determines which Nodes are valid placements for
each Pod in the scheduling queue according to constraints and available
resources. The scheduler then ranks each valid Node and binds the Pod to a
suitable Node. Multiple different schedulers may be used within a cluster;
kube-scheduler is the reference implementation.
See [scheduling](https://kubernetes.io/docs/concepts/scheduling-eviction/)
for more information about scheduling and the kube-scheduler component.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCommand(cmd, opts, registryOptions...)
		},
		Args: func(cmd *cobra.Command, args []string) error {
			for _, arg := range args {
				if len(arg) > 0 {
					return fmt.Errorf("%q does not take any arguments, got %q", cmd.CommandPath(), args)
				}
			}
			return nil
		},
	}

	nfs := opts.Flags
	// 设置一个 global的 flaset，后续可直接在map中获取 nfs.FlagSet("global")
	// 添加 version参数
	verflag.AddFlags(nfs.FlagSet("global"))
	// 添加 help参数
	globalflag.AddGlobalFlags(nfs.FlagSet("global"), cmd.Name(), logs.SkipLoggingConfigurationFlags())
	// 将 opts的 Flags 添加到 cmd的 flags
	fs := cmd.Flags()
	for _, f := range nfs.FlagSets {
		fs.AddFlagSet(f)
	}
	// 设置输出
	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cliflag.SetUsageAndHelpFunc(cmd, *nfs, cols)

	if err := cmd.MarkFlagFilename("config", "yaml", "yml", "json"); err != nil {
		klog.ErrorS(err, "Failed to mark flag filename")
	}

	return cmd
}

// runCommand runs the scheduler.
func runCommand(cmd *cobra.Command, opts *options.Options, registryOptions ...Option) error {
	// 验证版本信息
	verflag.PrintAndExitIfRequested()

	// Activate logging as soon as possible, after that
	// show flags with the final logging configuration.
	if err := logsapi.ValidateAndApply(opts.Logs, utilfeature.DefaultFeatureGate); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	// 打印参数
	cliflag.PrintFlags(cmd.Flags())
	// 上下文，信号的方式 来保证程序的退出
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		stopCh := server.SetupSignalHandler()
		<-stopCh
		cancel()
	}()
	// 进行默认配置
	cc, sched, err := Setup(ctx, opts, registryOptions...)
	if err != nil {
		return err
	}

	return Run(ctx, cc, sched)
}

// Run executes the scheduler based on the given configuration. It only returns on error or when context is done.
func Run(ctx context.Context, cc *schedulerserverconfig.CompletedConfig, sched *scheduler.Scheduler) error {
	// To help debugging, immediately log version
	klog.InfoS("Starting Kubernetes Scheduler", "version", version.Get())

	klog.InfoS("Golang settings", "GOGC", os.Getenv("GOGC"), "GOMAXPROCS", os.Getenv("GOMAXPROCS"), "GOTRACEBACK", os.Getenv("GOTRACEBACK"))

	// Configz registration.
	if cz, err := configz.New("componentconfig"); err == nil {
		cz.Set(cc.ComponentConfig)
	} else {
		return fmt.Errorf("unable to register configz: %s", err)
	}

	// Start events processing pipeline.
	// 启动事件 广播机制
	cc.EventBroadcaster.StartRecordingToSink(ctx.Done())
	defer cc.EventBroadcaster.Shutdown()

	// Setup healthz checks.
	//
	var checks []healthz.HealthChecker
	if cc.ComponentConfig.LeaderElection.LeaderElect {
		checks = append(checks, cc.LeaderElection.WatchDog)
	}

	waitingForLeader := make(chan struct{})
	isLeader := func() bool {
		select {
		case _, ok := <-waitingForLeader:
			// if channel is closed, we are leading
			return !ok
		default:
			// channel is open, we are waiting for a leader
			return false
		}
	}

	// Start up the healthz server.
	if cc.SecureServing != nil {
		handler := buildHandlerChain(newHealthzAndMetricsHandler(&cc.ComponentConfig, cc.InformerFactory, isLeader, checks...), cc.Authentication.Authenticator, cc.Authorization.Authorizer)
		// TODO: handle stoppedCh and listenerStoppedCh returned by c.SecureServing.Serve
		if _, _, err := cc.SecureServing.Serve(handler, 0, ctx.Done()); err != nil {
			// fail early for secure handlers, removing the old error loop from above
			return fmt.Errorf("failed to start secure server: %v", err)
		}
	}

	// Start all informers.
	// 开始informers
	cc.InformerFactory.Start(ctx.Done())
	// DynInformerFactory can be nil in tests.
	if cc.DynInformerFactory != nil {
		cc.DynInformerFactory.Start(ctx.Done())
	}

	// Wait for all caches to sync before scheduling.
	// 等待资源同步
	cc.InformerFactory.WaitForCacheSync(ctx.Done())
	// DynInformerFactory can be nil in tests.
	// 判断动态informer是否存在
	if cc.DynInformerFactory != nil {
		cc.DynInformerFactory.WaitForCacheSync(ctx.Done())
	}

	// If leader election is enabled, runCommand via LeaderElector until done and exit.
	// 如果启用了领导人选举，请通过LeaderElector运行Command，直到完成并退出。
	if cc.LeaderElection != nil {
		cc.LeaderElection.Callbacks = leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				close(waitingForLeader)
				sched.Run(ctx)
			},
			OnStoppedLeading: func() {
				select {
				case <-ctx.Done():
					// We were asked to terminate. Exit 0.
					klog.InfoS("Requested to terminate, exiting")
					os.Exit(0)
				default:
					// We lost the lock.
					klog.ErrorS(nil, "Leaderelection lost")
					klog.FlushAndExit(klog.ExitFlushTimeout, 1)
				}
			},
		}
		leaderElector, err := leaderelection.NewLeaderElector(*cc.LeaderElection)
		if err != nil {
			return fmt.Errorf("couldn't create leader elector: %v", err)
		}

		leaderElector.Run(ctx)

		return fmt.Errorf("lost lease")
	}

	// Leader election is disabled, so runCommand inline until done.
	// 领导人选举已禁用，因此请内联运行Command直到完成。
	close(waitingForLeader)
	sched.Run(ctx)
	return fmt.Errorf("finished without leader elect")
}

// buildHandlerChain wraps the given handler with the standard filters.
func buildHandlerChain(handler http.Handler, authn authenticator.Request, authz authorizer.Authorizer) http.Handler {
	requestInfoResolver := &apirequest.RequestInfoFactory{}
	failedHandler := genericapifilters.Unauthorized(scheme.Codecs)

	handler = genericapifilters.WithAuthorization(handler, authz, scheme.Codecs)
	handler = genericapifilters.WithAuthentication(handler, authn, failedHandler, nil)
	handler = genericapifilters.WithRequestInfo(handler, requestInfoResolver)
	handler = genericapifilters.WithCacheControl(handler)
	handler = genericfilters.WithHTTPLogging(handler)
	handler = genericfilters.WithPanicRecovery(handler, requestInfoResolver)

	return handler
}

func installMetricHandler(pathRecorderMux *mux.PathRecorderMux, informers informers.SharedInformerFactory, isLeader func() bool) {
	configz.InstallHandler(pathRecorderMux)
	pathRecorderMux.Handle("/metrics", legacyregistry.HandlerWithReset())

	resourceMetricsHandler := resources.Handler(informers.Core().V1().Pods().Lister())
	pathRecorderMux.HandleFunc("/metrics/resources", func(w http.ResponseWriter, req *http.Request) {
		if !isLeader() {
			return
		}
		resourceMetricsHandler.ServeHTTP(w, req)
	})
}

// newHealthzAndMetricsHandler creates a healthz server from the config, and will also
// embed the metrics handler.
func newHealthzAndMetricsHandler(config *kubeschedulerconfig.KubeSchedulerConfiguration, informers informers.SharedInformerFactory, isLeader func() bool, checks ...healthz.HealthChecker) http.Handler {
	pathRecorderMux := mux.NewPathRecorderMux("kube-scheduler")
	healthz.InstallHandler(pathRecorderMux, checks...)
	installMetricHandler(pathRecorderMux, informers, isLeader)
	if config.EnableProfiling {
		routes.Profiling{}.Install(pathRecorderMux)
		if config.EnableContentionProfiling {
			goruntime.SetBlockProfileRate(1)
		}
		routes.DebugFlags{}.Install(pathRecorderMux, "v", routes.StringFlagPutHandler(logs.GlogSetter))
	}
	return pathRecorderMux
}

func getRecorderFactory(cc *schedulerserverconfig.CompletedConfig) profile.RecorderFactory {
	return func(name string) events.EventRecorder {
		return cc.EventBroadcaster.NewRecorder(name)
	}
}

// WithPlugin creates an Option based on plugin name and factory. Please don't remove this function: it is used to register out-of-tree plugins,
// hence there are no references to it from the kubernetes scheduler code base.
func WithPlugin(name string, factory runtime.PluginFactory) Option {
	return func(registry runtime.Registry) error {
		return registry.Register(name, factory)
	}
}

// Setup creates a completed config and a scheduler based on the command args and options
func Setup(ctx context.Context, opts *options.Options, outOfTreeRegistryOptions ...Option) (*schedulerserverconfig.CompletedConfig, *scheduler.Scheduler, error) {
	// 版本配置
	if cfg, err := latest.Default(); err != nil {
		return nil, nil, err
	} else {
		opts.ComponentConfig = cfg
	}
	// opts的配置验证
	if errs := opts.Validate(); len(errs) > 0 {
		return nil, nil, utilerrors.NewAggregate(errs)
	}
	// 设置配置，包括生产kubeclient,EventBroadcaster,informar等
	c, err := opts.Config()
	if err != nil {
		return nil, nil, err
	}

	// Get the completed config
	// 添加向apiserver的认证
	cc := c.Complete()

	outOfTreeRegistry := make(runtime.Registry)
	// 这里默认是空的，因此不会进行操作
	for _, option := range outOfTreeRegistryOptions {
		if err := option(outOfTreeRegistry); err != nil {
			return nil, nil, err
		}
	}

	// 开启事件记录器
	recorderFactory := getRecorderFactory(&cc)
	completedProfiles := make([]kubeschedulerconfig.KubeSchedulerProfile, 0)
	// Create the scheduler.
	sched, err := scheduler.New(cc.Client,
		cc.InformerFactory,
		cc.DynInformerFactory,
		recorderFactory,
		ctx.Done(),
		// 相关配置
		// 设置组件版本
		scheduler.WithComponentConfigVersion(cc.ComponentConfig.TypeMeta.APIVersion),
		// 设置 kubeconfig 认证信息
		scheduler.WithKubeConfig(cc.KubeConfig),
		// 用户配置
		scheduler.WithProfiles(cc.ComponentConfig.Profiles...),
		// 节点得分百分比： 参数的实际意义是 选择合适的节点已经到达所有节点百分之比时，停止对后续节点进行选择，直接进行调度
		// 例如： 100个节点，我们配置30%，那么当打分节点到达30时，后续将不在进行打分，而是进入优选阶段
		scheduler.WithPercentageOfNodesToScore(cc.ComponentConfig.PercentageOfNodesToScore),
		// 使用外部插件
		scheduler.WithFrameworkOutOfTreeRegistry(outOfTreeRegistry),
		// 配置将调度失败的pod放到 backoff队列的时间 ，默认是 10秒 ，最长可以放到backoff队列多少秒，backoff队列不进行调度，也就是调度失败后，这10秒都不进行调度，
		// 这个时间是根据 初始时间，不断递增的， 计算公式是 initbackoff * 2的n次方 n是调度失败的次数
		scheduler.WithPodMaxBackoffSeconds(cc.ComponentConfig.PodMaxBackoffSeconds),
		// 配置将初始化失败的pod放到 backoff队列的时间 ，默认是 1秒，就是最初只放到backoff队列一秒 【backoff队列的pod是不进行调度的】
		scheduler.WithPodInitialBackoffSeconds(cc.ComponentConfig.PodInitialBackoffSeconds),
		// pod最大不进行调度的时间，遇到优先级高的pod进行优先调度，但是防止低优先级的pod一直不能得到调度，需要设置一个pod最长多长时间不进行调度
		scheduler.WithPodMaxInUnschedulablePodsDuration(cc.PodMaxInUnschedulablePodsDuration),
		// 扩展
		scheduler.WithExtenders(cc.ComponentConfig.Extenders...),
		scheduler.WithParallelism(cc.ComponentConfig.Parallelism),
		scheduler.WithBuildFrameworkCapturer(func(profile kubeschedulerconfig.KubeSchedulerProfile) {
			// Profiles are processed during Framework instantiation to set default plugins and configurations. Capturing them for logging
			completedProfiles = append(completedProfiles, profile)
		}),
	)
	if err != nil {
		return nil, nil, err
	}
	// 将 配置参数写入到 参数--write-config-to 对应的文件中
	if err := options.LogOrWriteConfig(opts.WriteConfigTo, &cc.ComponentConfig, completedProfiles); err != nil {
		return nil, nil, err
	}

	return &cc, sched, nil
}
