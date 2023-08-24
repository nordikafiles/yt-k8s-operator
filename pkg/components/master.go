package components

import (
	"context"
	"fmt"
	"strings"

	ytv1 "github.com/ytsaurus/yt-k8s-operator/api/v1"
	"go.ytsaurus.tech/yt/go/yson"

	"github.com/ytsaurus/yt-k8s-operator/pkg/apiproxy"
	"github.com/ytsaurus/yt-k8s-operator/pkg/consts"
	"github.com/ytsaurus/yt-k8s-operator/pkg/labeller"
	"github.com/ytsaurus/yt-k8s-operator/pkg/resources"
	"github.com/ytsaurus/yt-k8s-operator/pkg/ytconfig"
	corev1 "k8s.io/api/core/v1"
)

type master struct {
	ServerComponentBase

	initJob          *InitJob
	adminCredentials corev1.Secret
}

func NewMaster(cfgen *ytconfig.Generator, ytsaurus *apiproxy.Ytsaurus) ServerComponent {
	resource := ytsaurus.GetResource()
	l := labeller.Labeller{
		ObjectMeta:     &resource.ObjectMeta,
		APIProxy:       ytsaurus.APIProxy(),
		ComponentLabel: consts.YTComponentLabelMaster,
		ComponentName:  "Master",
		MonitoringPort: consts.MasterMonitoringPort,
	}

	server := NewServer(
		&l,
		ytsaurus,
		&resource.Spec.PrimaryMasters.InstanceSpec,
		"/usr/bin/ytserver-master",
		"ytserver-master.yson",
		cfgen.GetMastersStatefulSetName(),
		cfgen.GetMastersServiceName(),
		cfgen.GetMasterConfig,
		cfgen.NeedMasterConfigReload,
	)

	initJob := NewInitJob(
		&l,
		ytsaurus.APIProxy(),
		ytsaurus,
		resource.Spec.ImagePullSecrets,
		"default",
		consts.ClientConfigFileName,
		resource.Spec.CoreImage,
		cfgen.GetNativeClientConfig)

	return &master{
		ServerComponentBase: ServerComponentBase{
			ComponentBase: ComponentBase{
				labeller: &l,
				ytsaurus: ytsaurus,
				cfgen:    cfgen,
			},
			server: server,
		},
		initJob: initJob,
	}
}

func (m *master) Fetch(ctx context.Context) error {
	if m.ytsaurus.GetResource().Spec.AdminCredentials != nil {
		err := m.ytsaurus.APIProxy().FetchObject(
			ctx,
			m.ytsaurus.GetResource().Spec.AdminCredentials.Name,
			&m.adminCredentials)
		if err != nil {
			return err
		}
	}

	return resources.Fetch(ctx, []resources.Fetchable{
		m.server,
		m.initJob,
	})
}

func (m *master) initAdminUser() string {
	adminLogin, adminPassword := consts.DefaultAdminLogin, consts.DefaultAdminPassword
	adminToken := consts.DefaultAdminPassword

	if m.adminCredentials.Name != "" {
		value, ok := m.adminCredentials.Data[consts.AdminLoginSecret]
		if ok {
			adminLogin = string(value)
		}
		value, ok = m.adminCredentials.Data[consts.AdminPasswordSecret]
		if ok {
			adminPassword = string(value)
		}

		value, ok = m.adminCredentials.Data[consts.AdminTokenSecret]
		if ok {
			adminToken = string(value)
		}
	}

	commands := createUserCommand(adminLogin, adminPassword, adminToken, true)
	return strings.Join(commands, "\n")
}

func (m *master) initMedia() string {
	commands := []string{}
	for _, medium := range m.cfgen.GetExtraMedia() {
		attr, err := yson.MarshalFormat(medium, yson.FormatText)
		if err != nil {
			panic(err)
		}
		commands = append(commands, fmt.Sprintf("/usr/bin/yt get //sys/media/%s/@name || /usr/bin/yt create medium --attr '%s'", medium.Name, string(attr)))
	}
	return strings.Join(commands, "\n")
}

func (m *master) createInitScript() string {
	clusterConnection, err := m.cfgen.GetClusterConnection()
	if err != nil {
		panic(err)
	}

	script := []string{
		initJobWithNativeDriverPrologue(),
		"/usr/bin/yt remove //sys/@provision_lock -f",
		"/usr/bin/yt create scheduler_pool_tree --attributes '{name=default; config={nodes_filter=\"\"}}' --ignore-existing",
		"/usr/bin/yt set //sys/pool_trees/@default_tree default",
		"/usr/bin/yt create scheduler_pool --attributes '{name=research; pool_tree=default}' --ignore-existing",
		"/usr/bin/yt create map_node //home --ignore-existing",
		fmt.Sprintf("/usr/bin/yt set //sys/@cluster_connection '%s'", string(clusterConnection)),
		"/usr/bin/yt set //sys/controller_agents/config/operation_options/spec_template '{enable_partitioned_data_balancing=%false}' -r -f",
		m.initAdminUser(),
		m.initMedia(),
	}

	return strings.Join(script, "\n")
}

func (m *master) doSync(ctx context.Context, dry bool) (SyncStatus, error) {
	var err error

	if m.ytsaurus.GetClusterState() == ytv1.ClusterStateRunning && m.server.NeedUpdate() {
		return SyncStatusNeedFullUpdate, err
	}

	if m.ytsaurus.GetClusterState() == ytv1.ClusterStateUpdating {
		if m.ytsaurus.GetUpdateState() == ytv1.UpdateStateWaitingForPodsRemoval {
			updatingComponents := m.ytsaurus.GetLocalUpdatingComponents()
			if updatingComponents == nil {
				return SyncStatusUpdating, m.removePods(ctx, dry)
			}
		}
	}

	if m.server.NeedSync() {
		if !dry {
			err = m.server.Sync(ctx)
		}
		return SyncStatusPending, err
	}

	if !m.server.ArePodsReady(ctx) {
		return SyncStatusBlocked, err
	}

	if !dry {
		m.initJob.SetInitScript(m.createInitScript())
	}

	return m.initJob.Sync(ctx, dry)
}

func (m *master) Status(ctx context.Context) SyncStatus {
	status, err := m.doSync(ctx, true)
	if err != nil {
		panic(err)
	}

	return status
}

func (m *master) Sync(ctx context.Context) error {
	_, err := m.doSync(ctx, false)
	return err
}
