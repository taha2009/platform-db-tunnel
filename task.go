package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"unicode/utf16"
)

// A scheduled task is the alternative to the service for machines where the
// service's account can't be used (e.g. a Windows-auth user with no password):
// an "at log on" task borrows the signed-in user's session, so it needs no
// stored password, but it only runs while that user is signed in.

const taskTemplate = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>%[3]s</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>%[1]s</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>%[1]s</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>3</Count>
    </RestartOnFailure>
    <StartWhenAvailable>true</StartWhenAvailable>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%[2]s</Command>
      <Arguments>run</Arguments>
      <WorkingDirectory>%[4]s</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// utf16File encodes s as UTF-16LE with a BOM, which schtasks /XML requires
// when the document declares UTF-16.
func utf16File(s string) []byte {
	u := utf16.Encode([]rune(s))
	out := []byte{0xFF, 0xFE}
	for _, c := range u {
		out = append(out, byte(c), byte(c>>8))
	}
	return out
}

func schtasks(args ...string) error {
	out, err := exec.Command("schtasks.exe", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks %v: %w: %s", args[0], err, bytes.TrimSpace(out))
	}
	return nil
}

// installLogonTask registers a task that runs "<exe> run" whenever the invoking
// user signs in, then starts it now so a sign-out isn't needed first.
func installLogonTask() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	def := fmt.Sprintf(taskTemplate,
		xmlEscape(u.Username), xmlEscape(exe), xmlEscape(description), xmlEscape(filepath.Dir(exe)))

	f, err := os.CreateTemp("", "platform-db-tunnel-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(utf16File(def)); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	if err := schtasks("/Create", "/TN", serviceName, "/XML", f.Name(), "/F"); err != nil {
		return err
	}
	if err := schtasks("/Run", "/TN", serviceName); err != nil {
		return err
	}
	fmt.Printf("installed task %s for %s (starts at each sign-in) and started it\n", serviceName, u.Username)
	return nil
}

func uninstallLogonTask() error {
	schtasks("/End", "/TN", serviceName) // best effort; it may not be running
	if err := schtasks("/Delete", "/TN", serviceName, "/F"); err != nil {
		return err
	}
	fmt.Println("removed task", serviceName)
	return nil
}
