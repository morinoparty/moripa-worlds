package world

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/morinoparty/moripa-worlds/internal/schematic"
)

const testID = "0199c8a0-7b3e-7f12-9a4c-1d2e3f405162"

func testSettings() Settings {
	return Settings{
		Namespace:     "worlds",
		Image:         "ghcr.io/morinoparty/ianua-silvae:main",
		SchematicsPVC: "schematics",
		OriginY:       64,
		MemoryRequest: resource.MustParse("384Mi"),
		MemoryLimit:   resource.MustParse("768Mi"),
	}
}

func testInfo() *schematic.Info {
	title := "ロビーの噴水"
	return &schematic.Info{
		ID:            testID,
		Format:        "sponge_v3",
		FileName:      "schematic.schem",
		FileSize:      18234,
		Title:         &title,
		SpawnPosition: schematic.Position{X: 2.5, Y: 1, Z: -3.5, Yaw: 90, Pitch: 0},
		WorldSize:     schematic.Size{Width: 12, Height: 8, Length: 15},
		UploadedAt:    time.Date(2026, 10, 9, 11, 30, 0, 0, time.UTC),
	}
}

func wantLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "ianua-silvae",
		"app.kubernetes.io/component":  "world",
		"app.kubernetes.io/managed-by": "moripa-worlds",
		"worlds.morino.party/id":       testID,
	}
}

func checkMeta(t *testing.T, kind string, m metav1.ObjectMeta) {
	t.Helper()
	if m.Name != "w-"+testID || m.Namespace != "worlds" {
		t.Errorf("%s name/namespace = %s/%s", kind, m.Namespace, m.Name)
	}
	if !reflect.DeepEqual(m.Labels, wantLabels()) {
		t.Errorf("%s labels = %v", kind, m.Labels)
	}
	if _, ok := m.Labels["app.kubernetes.io/instance"]; ok {
		t.Errorf("%s must not have app.kubernetes.io/instance", kind)
	}
}

func TestConfigMap(t *testing.T) {
	cm, err := BuildConfigMap(testSettings(), testInfo(), nil)
	if err != nil {
		t.Fatal(err)
	}
	checkMeta(t, "ConfigMap", cm.ObjectMeta)
	if cm.Immutable == nil || !*cm.Immutable {
		t.Error("ConfigMap should be immutable")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(cm.Data["config.json"]), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"bind":            "0.0.0.0",
		"port":            float64(25565),
		"onlineMode":      true,
		"velocitySecret":  nil,
		"schematicPath":   "/schematics/" + testID + "/schematic.schem",
		"schematicOrigin": map[string]any{"x": 0.0, "y": 64.0, "z": 0.0, "yaw": 0.0, "pitch": 0.0},
		// spawn = origin + spawn_position (yaw / pitch はそのまま)
		"spawn": map[string]any{"x": 2.5, "y": 65.0, "z": -3.5, "yaw": 90.0, "pitch": 0.0},
		// 奈落は ianua-silvae が schematic の Offset を見て min(一番下のブロック, spawn.y) - 32 に置く
		"voidDepth": 32.0,
		// 古い ianua-silvae 向けの保険: min(64 + min(1, 0) - 8, -64) - 32
		"voidY": -96.0,
		"motd":  map[string]any{"description": "ロビーの噴水", "maxPlayers": float64(20)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("config.json =\n%v\nwant\n%v", got, want)
	}
}

func TestServerConfigDefaults(t *testing.T) {
	info := testInfo()
	info.Title = nil
	info.SpawnPosition = schematic.Position{}
	info.WorldSize.Height = 200
	c := BuildServerConfig(info, -10)
	if c.Motd.Description != "morino.party world" {
		t.Errorf("description = %q", c.Motd.Description)
	}
	if c.Spawn != (Position{X: 0, Y: -10, Z: 0}) || c.SchematicOrigin != (Position{Y: -10}) {
		t.Errorf("spawn = %+v origin = %+v", c.Spawn, c.SchematicOrigin)
	}
	// min(-10 + 0 - 200, -64) - 32
	if c.VoidY != -242 || c.VoidDepth != 32 {
		t.Errorf("voidY = %v voidDepth = %v", c.VoidY, c.VoidDepth)
	}
}

// //copy をビルドの上空で行った schematic (Offset.y が大きく負) でも、保険の voidY は
// ブロックが置かれうる最低の高さより下に来る。
func TestServerConfigVoidBelowDimension(t *testing.T) {
	info := testInfo()
	info.WorldSize.Height = 5
	info.SpawnPosition = schematic.Position{}
	c := BuildServerConfig(info, 64)
	if c.VoidY >= DimensionMinY {
		t.Errorf("voidY = %v, want below %d", c.VoidY, DimensionMinY)
	}
	// スポーンが貼り付け位置より下でも、スポーンから高さ分 + VoidDepth 下より上にはならない。
	info.SpawnPosition = schematic.Position{Y: -300}
	c = BuildServerConfig(info, 64)
	if c.VoidY != 64-300-5-VoidDepth {
		t.Errorf("voidY = %v", c.VoidY)
	}
}

func TestService(t *testing.T) {
	svc := BuildService(testSettings(), testID, nil)
	checkMeta(t, "Service", svc.ObjectMeta)
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("type = %s", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 25565 || svc.Spec.Ports[0].Name != "minecraft" {
		t.Errorf("ports = %+v", svc.Spec.Ports)
	}
	if svc.Spec.Selector["worlds.morino.party/id"] != testID {
		t.Errorf("selector = %v", svc.Spec.Selector)
	}
}

func TestDeployment(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	d := BuildDeployment(testSettings(), testInfo(), now)
	checkMeta(t, "Deployment", d.ObjectMeta)
	if d.Annotations[AnnotationLastActive] != "2026-10-09T12:00:00Z" {
		t.Errorf("last-active = %q", d.Annotations[AnnotationLastActive])
	}
	if *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != "Recreate" {
		t.Errorf("replicas/strategy = %d/%s", *d.Spec.Replicas, d.Spec.Strategy.Type)
	}
	if !reflect.DeepEqual(d.Spec.Template.Labels, wantLabels()) {
		t.Errorf("pod labels = %v", d.Spec.Template.Labels)
	}
	for k, v := range d.Spec.Selector.MatchLabels {
		if d.Spec.Template.Labels[k] != v {
			t.Errorf("selector %s=%s not in pod labels", k, v)
		}
	}

	pod := d.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must be false")
	}
	if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks {
		t.Error("enableServiceLinks must be false")
	}
	if *pod.TerminationGracePeriodSeconds != 15 {
		t.Errorf("terminationGracePeriodSeconds = %d", *pod.TerminationGracePeriodSeconds)
	}
	if psc := pod.SecurityContext; !*psc.RunAsNonRoot || *psc.RunAsUser != 10001 || *psc.RunAsGroup != 10001 || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod securityContext = %+v", psc)
	}

	// ボリューム
	vols := map[string]corev1.Volume{}
	for _, v := range pod.Volumes {
		vols[v.Name] = v
	}
	if pvc := vols["schematics"].PersistentVolumeClaim; pvc == nil || pvc.ClaimName != "schematics" || !pvc.ReadOnly {
		t.Errorf("schematics volume = %+v", vols["schematics"])
	}
	if cm := vols["config"].ConfigMap; cm == nil || cm.Name != "w-"+testID {
		t.Errorf("config volume = %+v", vols["config"])
	}
	if vols["tmp"].EmptyDir == nil {
		t.Errorf("tmp volume = %+v", vols["tmp"])
	}

	if len(pod.Containers) != 1 || len(pod.InitContainers) != 1 {
		t.Fatalf("containers = %d, init = %d", len(pod.Containers), len(pod.InitContainers))
	}
	c := pod.Containers[0]
	if c.Image != "ghcr.io/morinoparty/ianua-silvae:main" {
		t.Errorf("image = %s", c.Image)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		if e.ValueFrom != nil {
			continue
		}
		env[e.Name] = e.Value
	}
	if c.Env[0].Name != "IANUA_VELOCITY_SECRET" || c.Env[0].Value != "" || c.Env[0].ValueFrom == nil ||
		!reflect.DeepEqual(c.Env[0].ValueFrom.SecretKeyRef, &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "worlds-forwarding"}, Key: "secret",
		}) {
		t.Errorf("velocity secret env = %+v", c.Env[0])
	}
	wantEnv := map[string]string{
		"IANUA_ONLINE_MODE": "true",
		"IANUA_BIND":        "0.0.0.0",
		"IANUA_PORT":        "25565",
		"JAVA_TOOL_OPTIONS": "-XX:MaxRAMPercentage=75 -XX:+UseSerialGC -XX:+PerfDisableSharedMem",
	}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Errorf("env = %v", env)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = m
	}
	if m := mounts["/schematics"]; m.Name != "schematics" || !m.ReadOnly {
		t.Errorf("/schematics mount = %+v", m)
	}
	if m := mounts["/server/config"]; m.Name != "config" || !m.ReadOnly {
		t.Errorf("/server/config mount = %+v", m)
	}
	if m := mounts["/tmp"]; m.Name != "tmp" {
		t.Errorf("/tmp mount = %+v", m)
	}
	if c.StartupProbe == nil || c.StartupProbe.TCPSocket == nil || c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil {
		t.Error("startup / readiness probes must be tcpSocket")
	}
	if c.ReadinessProbe.TCPSocket.Port.String() != "minecraft" || c.Ports[0].ContainerPort != 25565 {
		t.Errorf("probe port = %s, container port = %d", c.ReadinessProbe.TCPSocket.Port.String(), c.Ports[0].ContainerPort)
	}
	if c.Resources.Requests.Cpu().String() != DefaultCPURequest || c.Resources.Requests.Memory().String() != "384Mi" || c.Resources.Limits.Memory().String() != "768Mi" {
		t.Errorf("resources = %+v", c.Resources)
	}
	if _, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		t.Error("no cpu limit expected")
	}

	for _, ctr := range []corev1.Container{c, pod.InitContainers[0]} {
		sc := ctr.SecurityContext
		if !*sc.RunAsNonRoot || *sc.RunAsUser != 10001 || *sc.RunAsGroup != 10001 || !*sc.ReadOnlyRootFilesystem ||
			*sc.AllowPrivilegeEscalation || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" ||
			sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("%s securityContext = %+v", ctr.Name, sc)
		}
	}

	if got := c.Resources.Requests.Cpu().String(); got != DefaultCPURequest {
		t.Errorf("cpu request = %s", got)
	}
	if _, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		t.Errorf("cpu limit should not be set: %+v", c.Resources.Limits)
	}
	if len(pod.TopologySpreadConstraints) != 1 {
		t.Fatalf("topologySpreadConstraints = %+v", pod.TopologySpreadConstraints)
	}
	tsc := pod.TopologySpreadConstraints[0]
	if tsc.TopologyKey != corev1.LabelHostname || tsc.WhenUnsatisfiable != corev1.ScheduleAnyway || tsc.MaxSkew != 1 ||
		tsc.LabelSelector.MatchLabels[LabelManagedBy] != ValueManagedBy || len(tsc.LabelSelector.MatchLabels) != 1 {
		t.Errorf("topologySpreadConstraint = %+v", tsc)
	}

	ic := pod.InitContainers[0]
	if ic.Image != "busybox:1.37" {
		t.Errorf("init image = %s", ic.Image)
	}
	if len(ic.Env) != 1 || ic.Env[0].Name != "SCHEMATIC" || ic.Env[0].Value != "/schematics/"+testID+"/schematic.schem" {
		t.Errorf("init env = %+v", ic.Env)
	}
	script := strings.Join(ic.Command, " ")
	if !strings.Contains(script, "-gt 120") || !strings.Contains(script, "exit 1") || strings.Contains(script, testID) {
		t.Errorf("init script = %q", script)
	}
	if len(ic.VolumeMounts) != 1 || !ic.VolumeMounts[0].ReadOnly {
		t.Errorf("init mounts = %+v", ic.VolumeMounts)
	}
}

func TestDeploymentForwardingSecretName(t *testing.T) {
	s := testSettings()
	s.ForwardingSecretName = "other-forwarding"
	d := BuildDeployment(s, testInfo(), time.Now())
	ref := d.Spec.Template.Spec.Containers[0].Env[0].ValueFrom.SecretKeyRef
	if ref.Name != "other-forwarding" || ref.Key != "secret" || ref.Optional != nil {
		t.Fatalf("secretKeyRef = %+v", ref)
	}
}

func TestDeploymentCPURequest(t *testing.T) {
	s := testSettings()
	s.CPURequest = resource.MustParse("500m")
	d := BuildDeployment(s, testInfo(), time.Now())
	if got := d.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().String(); got != "500m" {
		t.Errorf("cpu request = %s", got)
	}
}
