package dataplane

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

func TestNsenterCommanderBuildsHostNamespaceInvocation(t *testing.T) {
	c := NsenterCommander(1)
	cmd := c(context.Background(), "/usr/sbin/nginx", "-t", "-c", "/etc/nginx/x.conf")
	if cmd.Path == "" {
		t.Fatalf("command not built: %+v", cmd)
	}
	if cmd.Args[0] != "nsenter" {
		t.Fatalf("argv[0] = %q, want nsenter", cmd.Args[0])
	}
	want := []string{"nsenter", "-t", "1", "-n", "-m", "--", "/usr/sbin/nginx", "-t", "-c", "/etc/nginx/x.conf"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("args = %v, want %v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Fatalf("args = %v, want %v", cmd.Args, want)
		}
	}
}

// TestNginxClientOSCommanderSeam verifies the reload exec honours an
// injected Commander (the DaemonSet wrapper). A fake `nsenter` shim on a
// private PATH logs its argv and execs the command after `--`, so the test
// does not need real namespace privileges (the DaemonSet pod itself runs
// privileged — see charts/host-nginx-gateway).
func TestNginxClientOSCommanderSeam(t *testing.T) {
	dir := t.TempDir()
	nginxLog := dir + "/nginx.argv"
	nsenterLog := dir + "/nsenter.argv"

	fakeNginx := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + nginxLog + "\"\nexit 0\n"
	if err := os.WriteFile(dir+"/fake-nginx", []byte(fakeNginx), 0o755); err != nil {
		t.Fatal(err)
	}
	// The shim plays the role of util-linux nsenter: log everything, then
	// exec the command following the "--" separator.
	fakeNsenter := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + nsenterLog + "\"\n" +
		"while [ $# -gt 0 ]; do [ \"$1\" = \"--\" ] && shift && exec \"$@\"; shift; done\nexit 1\n"
	if err := os.WriteFile(dir+"/nsenter", []byte(fakeNsenter), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	n := &NginxClientOS{Binary: dir + "/fake-nginx", Commander: NsenterCommander(7)}
	if err := n.Reload(context.Background()); err != nil {
		t.Fatalf("reload through commander: %v", err)
	}
	nsArgs, _ := os.ReadFile(nsenterLog)
	if string(nsArgs) != "-t\n7\n-n\n-m\n--\n"+dir+"/fake-nginx\n-s\nreload\n" {
		t.Fatalf("nsenter argv = %q", nsArgs)
	}
	nginxArgs, _ := os.ReadFile(nginxLog)
	if string(nginxArgs) != "-s\nreload\n" {
		t.Fatalf("fake nginx argv = %q, want \"-s reload\" (the wrapper must be transparent)", nginxArgs)
	}
}

// TestValidatorCommanderSeam verifies `nginx -t` goes through the injected
// Commander.
func TestValidatorCommanderSeam(t *testing.T) {
	dir := t.TempDir()
	logPath := dir + "/argv.log"
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + logPath + "\"\necho ok >&2\nexit 0\n"
	fake := dir + "/fake-nginx"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	main := dir + "/nginx.conf"
	if err := os.WriteFile(main, []byte("http {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v := &Validator{
		NginxBinary:    fake,
		MainConfigPath: main,
		Prefix:         dir,
		// Inject a plain exec Commander through the seam (the production
		// wiring installs NsenterCommander; the seam contract is what is
		// under test here).
		Commander: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, name, args...)
		},
	}
	if _, err := v.Validate(context.Background(), main); err != nil {
		t.Fatalf("validate: %v", err)
	}
	got, _ := os.ReadFile(logPath)
	if len(got) == 0 {
		t.Fatal("validator did not exec through the Commander seam")
	}
}
