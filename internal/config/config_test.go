package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleConfig = `availabilityzones:
    - name: prod
      kubeconfig: /home/me/kubeconfig/prod
      context: admin@prod
    - name: test
      kubeconfig: /home/me/kubeconfig/test
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ctl.config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvVitiConfig, path)
	return path
}

// viti passes VITI_CONFIG to every plugin it dispatches, so "viti talos ..."
// must honour whatever config the parent viti was using rather than
// re-deriving one.
func TestVitistackConfigPathPrefersTheDispatchedConfig(t *testing.T) {
	t.Setenv(EnvVitiConfig, "/somewhere/else.yaml")
	got, err := VitistackConfigPath()
	if err != nil {
		t.Fatalf("VitistackConfigPath() error: %v", err)
	}
	if got != "/somewhere/else.yaml" {
		t.Errorf("VitistackConfigPath() = %q, want the dispatched config", got)
	}
}

func TestVitistackConfigPathFallsBackToHome(t *testing.T) {
	t.Setenv(EnvVitiConfig, "")
	got, err := VitistackConfigPath()
	if err != nil {
		t.Fatalf("VitistackConfigPath() error: %v", err)
	}
	if !strings.HasSuffix(got, filepath.Join(ConfigDirName, VitistackConfigFileName)) {
		t.Errorf("VitistackConfigPath() = %q, want ~/%s/%s", got, ConfigDirName, VitistackConfigFileName)
	}
}

func TestAvailabilityZonesReadsEveryZone(t *testing.T) {
	writeConfig(t, sampleConfig)
	t.Setenv(EnvAvailabilityZone, "")

	zones, err := AvailabilityZones("")
	if err != nil {
		t.Fatalf("AvailabilityZones() error: %v", err)
	}
	if len(zones) != 2 || zones[0].Name != "prod" || zones[0].Context != "admin@prod" {
		t.Errorf("AvailabilityZones() = %+v", zones)
	}
}

func TestAvailabilityZonesNarrowsByName(t *testing.T) {
	writeConfig(t, sampleConfig)
	t.Setenv(EnvAvailabilityZone, "")

	zones, err := AvailabilityZones("test")
	if err != nil {
		t.Fatalf("AvailabilityZones() error: %v", err)
	}
	if len(zones) != 1 || zones[0].Name != "test" {
		t.Errorf("AvailabilityZones(\"test\") = %+v", zones)
	}
	if _, err := AvailabilityZones("nope"); err == nil {
		t.Error("AvailabilityZones() accepted an unknown zone")
	}
}

// "viti -z prod talos ..." reaches the plugin as an env var, so a zone-scoped
// invocation has to stay zone-scoped.
func TestAvailabilityZonesHonoursTheForwardedZoneFlag(t *testing.T) {
	writeConfig(t, sampleConfig)
	t.Setenv(EnvAvailabilityZone, "prod")

	zones, err := AvailabilityZones("")
	if err != nil {
		t.Fatalf("AvailabilityZones() error: %v", err)
	}
	if len(zones) != 1 || zones[0].Name != "prod" {
		t.Errorf("AvailabilityZones() = %+v, want only prod", zones)
	}
}

// An explicit argument beats the forwarded default, so a plugin subcommand can
// still widen or change the zone it was dispatched with.
func TestExplicitZoneBeatsTheEnvironment(t *testing.T) {
	writeConfig(t, sampleConfig)
	t.Setenv(EnvAvailabilityZone, "prod")

	zones, err := AvailabilityZones("test")
	if err != nil {
		t.Fatalf("AvailabilityZones() error: %v", err)
	}
	if len(zones) != 1 || zones[0].Name != "test" {
		t.Errorf("AvailabilityZones(\"test\") = %+v, want test", zones)
	}
}

// The fix differs between "no config at all" and "a config with no zones", so
// the two messages must differ.
func TestAvailabilityZonesErrorsAreActionable(t *testing.T) {
	t.Setenv(EnvVitiConfig, filepath.Join(t.TempDir(), "missing.yaml"))
	_, err := AvailabilityZones("")
	if err == nil || !strings.Contains(err.Error(), "viti config") {
		t.Errorf("missing config error = %v, want it to name the command that fixes it", err)
	}

	writeConfig(t, "availabilityzones: []\n")
	_, err = AvailabilityZones("")
	if err == nil || !strings.Contains(err.Error(), "no availability zones configured") {
		t.Errorf("empty config error = %v", err)
	}
}

func TestAvailabilityZonesReportsBrokenYAML(t *testing.T) {
	writeConfig(t, "availabilityzones: [oops\n")
	if _, err := AvailabilityZones(""); err == nil {
		t.Error("AvailabilityZones() accepted unparseable YAML")
	}
}
