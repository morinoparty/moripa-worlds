// Package world はワールド (ianua-silvae の Deployment / Service / ConfigMap) の
// 組み立て・作成・削除と、使われなくなったワールドを片付ける reaper を提供する。
package world

import (
	"encoding/json"
	"fmt"
	"path"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/morinoparty/moripa-worlds/internal/schematic"
)

// ラベル・アノテーションのキーと値。
const (
	LabelName      = "app.kubernetes.io/name"
	LabelComponent = "app.kubernetes.io/component"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelID        = "worlds.morino.party/id"

	ValueName      = "ianua-silvae"
	ValueComponent = "world"
	ValueManagedBy = "moripa-worlds"

	// AnnotationLastActive はプレイヤーが最後にいた時刻 (RFC 3339)。
	// コントローラーが再起動してもアイドル判定を引き継ぐために Deployment に記録する。
	AnnotationLastActive = "worlds.morino.party/last-active"
	// AnnotationPlayed は一度でもプレイヤーがいたワールドに付ける ("true")。
	// 付いていないワールドは IDLE_TIMEOUT ではなく短い UNJOINED_TIMEOUT で片付ける。
	AnnotationPlayed = "worlds.morino.party/played"

	// ManagedSelector は moripa-worlds が作ったオブジェクトだけを選ぶラベルセレクター。
	ManagedSelector = LabelManagedBy + "=" + ValueManagedBy

	// ConfigKey は ConfigMap 内の ianua-silvae 設定ファイルのキー。
	ConfigKey = "config.json"
	// Port は ianua-silvae の待ち受けポート。
	Port = 25565
	// PortName は Service / コンテナのポート名。
	PortName = "minecraft"
	// UID は ianua-silvae イメージの実行ユーザー / グループ。
	UID = 10001

	// SchematicsMountPath は schematic の PVC (RustFS の moripa-utils/schematics) のマウント先。
	SchematicsMountPath = "/schematics"
	// ConfigMountPath は ianua-silvae の作業ディレクトリ /server からの config/ 。
	ConfigMountPath = "/server/config"

	// InitImage は schematic の出現を待つ initContainer のイメージ。
	InitImage = "busybox:1.37"
	// SchematicWaitSeconds は initContainer が schematic 本体を待つ最大秒数 (geesefs は一覧を ~10 秒キャッシュする)。
	SchematicWaitSeconds = 120

	// MaxPlayers はワールドの最大人数 (ianua-silvae の motd.maxPlayers)。
	MaxPlayers = 20
	// JavaToolOptions は ianua-silvae の JVM オプション。
	JavaToolOptions = "-XX:MaxRAMPercentage=75 -XX:+UseSerialGC -XX:+PerfDisableSharedMem"
)

// Settings はワールドの組み立てに使う設定。
type Settings struct {
	Namespace     string
	Image         string
	SchematicsPVC string
	OriginY       int
	MemoryRequest resource.Quantity
	MemoryLimit   resource.Quantity
}

// Name はワールド ID からオブジェクト名 (w-<uuid>) を作る。
func Name(id string) string { return "w-" + id }

// Labels はワールドのオブジェクトすべてに付けるラベル。
// ArgoCD の追跡対象にならないよう app.kubernetes.io/instance は付けない。
func Labels(id string) map[string]string {
	return map[string]string{
		LabelName:      ValueName,
		LabelComponent: ValueComponent,
		LabelManagedBy: ValueManagedBy,
		LabelID:        id,
	}
}

func selectorLabels(id string) map[string]string {
	return map[string]string{LabelName: ValueName, LabelID: id}
}

// Position は ianua-silvae の config.json の位置。
type Position struct {
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Z     float64 `json:"z"`
	Yaw   float64 `json:"yaw"`
	Pitch float64 `json:"pitch"`
}

// Motd は ianua-silvae の config.json の motd。
type Motd struct {
	Description string `json:"description"`
	MaxPlayers  int    `json:"maxPlayers"`
}

// ServerConfig は ianua-silvae の config/config.json。
type ServerConfig struct {
	Bind            string   `json:"bind"`
	Port            int      `json:"port"`
	OnlineMode      bool     `json:"onlineMode"`
	VelocitySecret  *string  `json:"velocitySecret"`
	SchematicPath   string   `json:"schematicPath"`
	SchematicOrigin Position `json:"schematicOrigin"`
	Spawn           Position `json:"spawn"`
	VoidY           float64  `json:"voidY"`
	Motd            Motd     `json:"motd"`
}

// SchematicPath はコンテナ内の schematic 本体のパス。
func SchematicPath(info *schematic.Info) string {
	return path.Join(SchematicsMountPath, info.ID, info.FileName)
}

// BuildServerConfig は info.json から ianua-silvae の設定を組み立てる。
// schematic は (0, originY, 0) に貼り付け、スポーンは貼り付け位置 + spawn_position、
// voidY は schematic の高さの 32 ブロック下とする。
func BuildServerConfig(info *schematic.Info, originY int) ServerConfig {
	origin := Position{X: 0, Y: float64(originY), Z: 0}
	sp := info.SpawnPosition
	return ServerConfig{
		Bind:            "0.0.0.0",
		Port:            Port,
		OnlineMode:      true,
		VelocitySecret:  nil,
		SchematicPath:   SchematicPath(info),
		SchematicOrigin: origin,
		Spawn: Position{
			X: origin.X + sp.X, Y: origin.Y + sp.Y, Z: origin.Z + sp.Z,
			Yaw: sp.Yaw, Pitch: sp.Pitch,
		},
		VoidY: float64(originY - info.WorldSize.Height - 32),
		Motd:  Motd{Description: info.DisplayTitle(), MaxPlayers: MaxPlayers},
	}
}

func objectMeta(s Settings, id string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: Name(id), Namespace: s.Namespace, Labels: Labels(id)}
}

// OwnerReference は Deployment を所有者とする参照を作る。
// ConfigMap / Service に付けておくと、Deployment を消したときに GC でも消える。
func OwnerReference(d *appsv1.Deployment) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         "apps/v1",
		Kind:               "Deployment",
		Name:               d.Name,
		UID:                d.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(false),
	}
}

// BuildConfigMap は ianua-silvae の config.json を入れた ConfigMap を作る。
func BuildConfigMap(s Settings, info *schematic.Info, owner *metav1.OwnerReference) (*corev1.ConfigMap, error) {
	b, err := json.MarshalIndent(BuildServerConfig(info, s.OriginY), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("world: marshal config.json: %w", err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: objectMeta(s, info.ID),
		Immutable:  ptr.To(true),
		Data:       map[string]string{ConfigKey: string(b)},
	}
	if owner != nil {
		cm.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return cm, nil
}

// BuildService はワールドの ClusterIP Service を作る。
func BuildService(s Settings, id string, owner *metav1.OwnerReference) *corev1.Service {
	svc := &corev1.Service{
		ObjectMeta: objectMeta(s, id),
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selectorLabels(id),
			Ports: []corev1.ServicePort{{
				Name:       PortName,
				Port:       Port,
				TargetPort: intstr.FromString(PortName),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
	if owner != nil {
		svc.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return svc
}

// waitScript は schematic 本体が geesefs 上に見えるまで最大 SchematicWaitSeconds 秒待つ。
// パスは環境変数 SCHEMATIC で渡す (シェルに文字列を埋め込まない)。
var waitScript = fmt.Sprintf(`i=0
while [ ! -f "$SCHEMATIC" ]; do
  i=$((i+1))
  if [ "$i" -gt %d ]; then
    echo "schematic not found after %ds: $SCHEMATIC" >&2
    exit 1
  fi
  sleep 1
done
echo "schematic found: $SCHEMATIC"
`, SchematicWaitSeconds, SchematicWaitSeconds)

func containerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             ptr.To(true),
		RunAsUser:                ptr.To[int64](UID),
		RunAsGroup:               ptr.To[int64](UID),
		ReadOnlyRootFilesystem:   ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// BuildDeployment はワールドの Deployment を作る。now は last-active の初期値。
func BuildDeployment(s Settings, info *schematic.Info, now time.Time) *appsv1.Deployment {
	id := info.ID
	meta := objectMeta(s, id)
	meta.Annotations = map[string]string{AnnotationLastActive: now.UTC().Format(time.RFC3339)}
	tcp := corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString(PortName)}}

	return &appsv1.Deployment{
		ObjectMeta: meta,
		Spec: appsv1.DeploymentSpec{
			Replicas:             ptr.To[int32](1),
			RevisionHistoryLimit: ptr.To[int32](1),
			Strategy:             appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector:             &metav1.LabelSelector{MatchLabels: selectorLabels(id)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: Labels(id)},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken:  ptr.To(false),
					EnableServiceLinks:            ptr.To(false),
					TerminationGracePeriodSeconds: ptr.To[int64](15),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr.To(true),
						RunAsUser:      ptr.To[int64](UID),
						RunAsGroup:     ptr.To[int64](UID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					InitContainers: []corev1.Container{{
						Name:            "wait-schematic",
						Image:           InitImage,
						Command:         []string{"sh", "-c", waitScript},
						Env:             []corev1.EnvVar{{Name: "SCHEMATIC", Value: SchematicPath(info)}},
						SecurityContext: containerSecurityContext(),
						VolumeMounts: []corev1.VolumeMount{
							{Name: "schematics", MountPath: SchematicsMountPath, ReadOnly: true},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("16Mi"),
							},
							Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("32Mi")},
						},
					}},
					Containers: []corev1.Container{{
						Name:  "server",
						Image: s.Image,
						Env: []corev1.EnvVar{
							{Name: "IANUA_ONLINE_MODE", Value: "true"},
							{Name: "IANUA_BIND", Value: "0.0.0.0"},
							{Name: "IANUA_PORT", Value: fmt.Sprint(Port)},
							{Name: "JAVA_TOOL_OPTIONS", Value: JavaToolOptions},
						},
						Ports: []corev1.ContainerPort{{Name: PortName, ContainerPort: Port, Protocol: corev1.ProtocolTCP}},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "schematics", MountPath: SchematicsMountPath, ReadOnly: true},
							{Name: "config", MountPath: ConfigMountPath, ReadOnly: true},
							{Name: "tmp", MountPath: "/tmp"},
						},
						StartupProbe: &corev1.Probe{
							ProbeHandler:     tcp,
							PeriodSeconds:    2,
							FailureThreshold: 90,
							TimeoutSeconds:   1,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:     tcp,
							PeriodSeconds:    5,
							FailureThreshold: 3,
							TimeoutSeconds:   1,
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: s.MemoryRequest,
							},
							Limits: corev1.ResourceList{corev1.ResourceMemory: s.MemoryLimit},
						},
						SecurityContext: containerSecurityContext(),
					}},
					Volumes: []corev1.Volume{
						{Name: "schematics", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: s.SchematicsPVC, ReadOnly: true,
						}}},
						{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: Name(id)},
						}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
							SizeLimit: ptr.To(resource.MustParse("64Mi")),
						}}},
					},
				},
			},
		},
	}
}
