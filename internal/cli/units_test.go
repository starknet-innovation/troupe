package cli

import (
	"regexp"
	"strings"
	"testing"
)

func TestUnitsFillEveryPlaceholder(t *testing.T) {
	names, err := unitNames()
	if err != nil || len(names) == 0 {
		t.Fatalf("units %v err %v", names, err)
	}
	for _, name := range names {
		text, err := fillUnit(name, "/opt/bin/troupe", "operator", "/home/operator")
		if err != nil {
			t.Fatal(err)
		}
		for _, placeholder := range []string{"@TROUPE@", "@USER@", "@HOME@"} {
			if strings.Contains(text, placeholder) {
				t.Errorf("%s keeps %s", name, placeholder)
			}
		}
		if !strings.Contains(text, "\nUser=operator\n") {
			t.Errorf("%s does not run as the user", name)
		}
	}
}

// Every troupe command a unit runs must exist in this build, or systemd would run a unit
// whose start condition or stop step can never succeed.
func TestUnitsCallOnlyExistingCommands(t *testing.T) {
	call := regexp.MustCompile(`@TROUPE@ ([a-z]+)`)
	names, _ := unitNames()
	seen := 0
	for _, name := range names {
		template, _ := fillUnit(name, "@TROUPE@", "u", "/h")
		for _, match := range call.FindAllStringSubmatch(template, -1) {
			seen++
			if _, ok := commands[match[1]]; !ok {
				t.Errorf("%s runs troupe %s, which does not exist", name, match[1])
			}
		}
	}
	if seen == 0 {
		t.Fatal("no unit runs troupe: the check sees nothing")
	}
}

func TestDaemonUnitWaitsForTheProbe(t *testing.T) {
	daemon, err := fillUnit("sbx-daemon.service", "/opt/bin/troupe", "operator", "/home/operator")
	if err != nil || !strings.Contains(daemon, "\nExecStartPost=/opt/bin/troupe guard probe --wait 75s\n") {
		t.Fatalf("daemon unit does not wait for the probe (%v)", err)
	}
}

func TestUnitsRefuseRootTemporaryBinariesAndUnknownNames(t *testing.T) {
	old := geteuid
	geteuid = func() int { return 0 }
	if _, err := renderUnit("sbx-daemon.service"); err == nil {
		t.Error("rendered as root")
	}
	geteuid = old
	// go test runs this binary from a temporary build folder: exactly the case to refuse.
	if _, err := renderUnit("sbx-daemon.service"); err == nil || !strings.Contains(err.Error(), "temporary folder") {
		t.Errorf("rendered from a temporary binary: %v", err)
	}
	if _, err := renderUnit("other.service"); err == nil {
		t.Error("rendered an unknown unit")
	}
}

func TestTransientBinaryPaths(t *testing.T) {
	for path, want := range map[string]bool{
		"/tmp/go-build123/b001/exe/troupe":          true,
		"/home/op/.cache/go-build/ee/eebc-d/troupe": true,
		"/home/op/.local/bin/troupe":                false,
		"/usr/local/bin/troupe":                     false,
		"/home/op/src/troupe/troupe":                false,
	} {
		if got := transient(path); got != want {
			t.Errorf("transient(%q) = %v, want %v", path, got, want)
		}
	}
}

// The start condition (skip a suspended agent) runs before anything that could touch the guest.
func TestGatewayStartConditionPrecedesTheProbeAndStart(t *testing.T) {
	text, err := fillUnit("hermes-gateway@.service", "/opt/bin/troupe", "operator", "/home/operator")
	if err != nil {
		t.Fatal(err)
	}
	condition, pre, start := strings.Index(text, "\nExecCondition="), strings.Index(text, "\nExecStartPre="), strings.Index(text, "\nExecStart=")
	if condition < 0 || !(condition < pre && pre < start) {
		t.Fatalf("order: condition %d, pre %d, start %d", condition, pre, start)
	}
	for _, want := range []string{
		"\nExecCondition=/opt/bin/troupe guard check %i\n",
		"\nExecStartPre=/opt/bin/troupe guard probe\n",
		"\nExecStop=/opt/bin/troupe gateway stop %i\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("gateway unit lacks %q", strings.TrimSpace(want))
		}
	}
}
