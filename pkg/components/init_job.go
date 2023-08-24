package components

import (
	"context"
	"fmt"
	"path"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ytsaurus/yt-k8s-operator/pkg/apiproxy"
	"github.com/ytsaurus/yt-k8s-operator/pkg/consts"
	"github.com/ytsaurus/yt-k8s-operator/pkg/labeller"
	"github.com/ytsaurus/yt-k8s-operator/pkg/resources"
	"github.com/ytsaurus/yt-k8s-operator/pkg/ytconfig"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const initJobPrologue = `
set -e
set -x
`

func initJobWithNativeDriverPrologue() string {
	commands := []string{
		initJobPrologue,
		fmt.Sprintf("export YT_DRIVER_CONFIG_PATH=%s", path.Join(consts.ConfigMountPoint, consts.ClientConfigFileName)),
	}
	return strings.Join(commands, "\n")
}

type InitJob struct {
	ComponentBase
	apiProxy          apiproxy.APIProxy
	conditionsManager apiproxy.ConditionManager
	imagePullSecrets  []corev1.LocalObjectReference

	initJob *resources.Job

	configHelper           *ConfigHelper
	initCompletedCondition string

	image string

	builtJob *batchv1.Job
}

func NewInitJob(
	labeller *labeller.Labeller,
	apiProxy apiproxy.APIProxy,
	conditionsManager apiproxy.ConditionManager,
	imagePullSecrets []corev1.LocalObjectReference,
	name, configFileName, image string,
	generator ytconfig.GeneratorFunc) *InitJob {
	return &InitJob{
		ComponentBase: ComponentBase{
			labeller: labeller,
		},
		apiProxy:               apiProxy,
		conditionsManager:      conditionsManager,
		imagePullSecrets:       imagePullSecrets,
		initCompletedCondition: fmt.Sprintf("%s%sInitJobCompleted", name, labeller.ComponentName),
		image:                  image,
		initJob: resources.NewJob(
			labeller.GetInitJobName(name),
			labeller,
			apiProxy),
		configHelper: NewConfigHelper(
			labeller,
			apiProxy,
			fmt.Sprintf(
				"%s-%s-init-job-config",
				strings.ToLower(name),
				labeller.ComponentLabel),
			configFileName,
			nil,
			generator,
			nil),
	}
}

func (j *InitJob) SetInitScript(script string) {
	cm := j.configHelper.Build()
	cm.Data[consts.InitClusterScriptFileName] = script
}

func (j *InitJob) Build() *batchv1.Job {
	if j.builtJob != nil {
		return j.builtJob
	}
	var defaultMode int32 = 0500
	job := j.initJob.Build()
	job.Spec.Template = corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			ImagePullSecrets: j.imagePullSecrets,
			Containers: []corev1.Container{
				{
					Image:   j.image,
					Name:    "ytsaurus-init",
					Command: []string{"bash", "-c", path.Join(consts.ConfigMountPoint, consts.InitClusterScriptFileName)},
					VolumeMounts: []corev1.VolumeMount{
						createConfigVolumeMount(),
					},
				},
			},
			Volumes: []corev1.Volume{
				createConfigVolume(j.configHelper.GetConfigMapName(), &defaultMode),
			},
			RestartPolicy: corev1.RestartPolicyOnFailure,
		},
	}
	j.builtJob = job
	return job
}

func (j *InitJob) Fetch(ctx context.Context) error {
	return resources.Fetch(ctx, []resources.Fetchable{
		j.initJob,
		j.configHelper,
	})
}

func (j *InitJob) Sync(ctx context.Context, dry bool) (SyncStatus, error) {
	logger := log.FromContext(ctx)
	var err error

	if j.conditionsManager.IsStatusConditionTrue(j.initCompletedCondition) {
		return SyncStatusReady, err
	}

	// Deal with init job.
	if !resources.Exists(j.initJob) {
		if dry {
			return SyncStatusPending, nil
		}
		_ = j.Build()
		err = resources.Sync(ctx, []resources.Syncable{
			j.configHelper,
			j.initJob,
		})
		return SyncStatusPending, err
	}

	if !j.initJob.Completed() {
		logger.Info("Init job is not completed for " + j.labeller.ComponentName)
		return SyncStatusBlocked, err
	}

	if !dry {
		err = j.conditionsManager.SetStatusCondition(ctx, metav1.Condition{
			Type:    j.initCompletedCondition,
			Status:  metav1.ConditionTrue,
			Reason:  "InitJobCompleted",
			Message: "Init job successfully completed",
		})
	}

	return SyncStatusPending, err
}

func (j *InitJob) prepareRestart(ctx context.Context, dry bool) error {
	if dry {
		return nil
	}
	if err := j.removeIfExists(ctx); err != nil {
		return err
	}
	return j.conditionsManager.SetStatusCondition(ctx, metav1.Condition{
		Type:    j.initCompletedCondition,
		Status:  metav1.ConditionFalse,
		Reason:  "InitJobNeedRestart",
		Message: "Init job needs restart",
	})
}

func (j *InitJob) isRestartPrepared() bool {
	return !resources.Exists(j.initJob) && j.conditionsManager.IsStatusConditionFalse(j.initCompletedCondition)
}

func (j *InitJob) isRestartCompleted() bool {
	return j.conditionsManager.IsStatusConditionTrue(j.initCompletedCondition)
}

func (j *InitJob) removeIfExists(ctx context.Context) error {
	if !resources.Exists(j.initJob) {
		return nil
	}
	propagation := metav1.DeletePropagationForeground
	return j.apiProxy.DeleteObject(
		ctx,
		j.initJob.OldObject(),
		&client.DeleteOptions{PropagationPolicy: &propagation},
	)
}
