package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	hyperv1alpha1 "github.com/taha/myprog/hyper-operator/api/v1alpha1"
	"github.com/taha/myprog/internal/config"
	"github.com/taha/myprog/internal/redis"
)

type HyperRedisReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

func (r *HyperRedisReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var hyperRedis hyperv1alpha1.HyperRedis
	if err := r.Get(ctx, req.NamespacedName, &hyperRedis); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch HyperRedis")
		return ctrl.Result{}, err
	}

	if !hyperRedis.Spec.ActiveConnHealthCheck {
		return ctrl.Result{}, nil
	}

	timeoutDur, err := time.ParseDuration(hyperRedis.Spec.Timeout)
	if err != nil || timeoutDur == 0 {
		timeoutDur = 5 * time.Second
	}
	startupElapsed, _ := time.ParseDuration("2s")

	mappedConfig := config.RedisServiceConfig{
		URL:                            hyperRedis.Spec.Url,
		Type:                           string(hyperRedis.Spec.Type),
		PoolSize:                       hyperRedis.Spec.PoolSize,
		Timeout:                        hyperRedis.Spec.Timeout,
		TimeoutDuration:                timeoutDur,
		StartupMaxElapsedTime:          "2s",
		StartupMaxElapsedTimeDuration:  startupElapsed,
		StartupInitialIntervalDuration: 500 * time.Millisecond,
		StartupMaxIntervalDuration:     1 * time.Second,
	}

	redisClient, err := redis.NewClientConn(ctx, req.Name, mappedConfig)

	if err != nil {
		logger.Error(err, "Connection/PING failed")

		hyperRedis.Status.State = hyperv1alpha1.RedisStateError
		hyperRedis.Status.LastCheck = metav1.Now()

		if updateErr := r.Status().Update(ctx, &hyperRedis); updateErr != nil {
			return ctrl.Result{}, updateErr
		}

		r.Recorder.Event(&hyperRedis, "Warning", "ConnectionFailed", err.Error())

		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	defer func() {
		_ = redisClient.Close()
	}()

	hyperRedis.Status.State = hyperv1alpha1.RedisStateConnected
	hyperRedis.Status.LastCheck = metav1.Now()

	if updateErr := r.Status().Update(ctx, &hyperRedis); updateErr != nil {
		return ctrl.Result{}, updateErr
	}

	r.Recorder.Event(&hyperRedis, "Normal", "ConnectionEstablished", "Successfully pinged Redis")

	return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
}

func (r *HyperRedisReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&hyperv1alpha1.HyperRedis{}).
		Complete(r)
}