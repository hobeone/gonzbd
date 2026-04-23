//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/sabnzbd-go/internal/nntp/nntptest"
	"github.com/hobeone/sabnzbd-go/test/mocknntp"
)

func TestIntegration_OneShotCLI(t *testing.T) {
	// 1. Setup mock server and dummy NZB
	srv := nntptest.New(t)
	payload := []byte("oneshot integration test")
	msgID := "oneshot-cli@test"
	srv.AddArticle(msgID, mocknntp.EncodeYEnc("oneshot.bin", payload))

	nzbContent := fmt.Sprintf(`<?xml version="1.0" encoding="iso-8859-1" ?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
 <file poster="tester" date="1700000000" subject="oneshot.bin">
  <groups><group>alt.binaries.test</group></groups>
  <segments>
   <segment bytes="%d" number="1">%s</segment>
  </segments>
 </file>
</nzb>`, len(payload), msgID)

	dir := t.TempDir()
	nzbPath := filepath.Join(dir, "test.nzb")
	if err := os.WriteFile(nzbPath, []byte(nzbContent), 0644); err != nil {
		t.Fatalf("failed to write dummy nzb: %v", err)
	}

	// 2. Create a minimal valid config
	configContent := `
general:
  host: 127.0.0.1
  port: 4289
  api_key: aaaaaaaaaaaaaaaa
  nzb_key: bbbbbbbbbbbbbbbb
  download_dir: ` + dir + `
  admin_dir: ` + dir + `
  complete_dir: ` + dir + `
downloads:
  max_art_tries: 3
postproc:
  par2_command: par2
servers:
  - name: test
    host: ` + strings.Split(srv.Addr(), ":")[0] + `
    port: ` + strings.Split(srv.Addr(), ":")[1] + `
    connections: 1
    timeout: 60
    pipelining_requests: 1
    enable: true
`
	configPath := filepath.Join(dir, "sabnzbd.yaml")
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("failed to write dummy config: %v", err)
	}

	// 3. Build and run the binary
	binary := filepath.Join(dir, "sabnzbd")
	buildCmd := exec.Command("go", "build", "-o", binary, "../../cmd/sabnzbd")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build binary: %v\n%s", err, out)
	}

	runCmd := exec.Command(binary, "--config", configPath, "--nzb", nzbPath)
	out, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("CLI run failed: %v\n%s", err, string(out))
	}

	// 4. Verify the output contains the performance summary
	output := string(out)
	expectedLines := []string{
		"--- Download Summary ---",
		"Job:        test",
		"Status:     Completed",
		"Location:",
		"Avg Network:",
		"Avg Disk:",
	}

	for _, line := range expectedLines {
		if !strings.Contains(output, line) {
			t.Errorf("output missing expected line %q\nFull output:\n%s", line, output)
		}
	}
}
