// Package service provides service installer and management for Shepherd daemon.
// Supports Linux systemd user units and macOS launchd user agents (brief §6, §12).
package service

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// GenerateSystemdUnit returns the systemd user service unit content for Shepherd.
func GenerateSystemdUnit(execPath string, workingDir string) string {
	return fmt.Sprintf(`[Unit]
Description=Shepherd AI Daemon
After=network.target

[Service]
Type=simple
WorkingDirectory=%s
ExecStart=%s start -f
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=default.target
`, workingDir, execPath)
}

// GenerateLaunchdPlist returns the launchd user agent plist content for Shepherd.
func GenerateLaunchdPlist(execPath string, workingDir string, logDir string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>ai.shepherd.daemon</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>start</string>
        <string>-f</string>
    </array>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s/shepherd.log</string>
    <key>StandardErrorPath</key>
    <string>%s/shepherd.err.log</string>
</dict>
</plist>
`, execPath, workingDir, logDir, logDir)
}

// SystemdUnitPath returns the default user systemd unit path for the current user.
func SystemdUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", "shepherd.service"), nil
}

// LaunchdPlistPath returns the default user launchd agent path for the current user.
func LaunchdPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", "ai.shepherd.daemon.plist"), nil
}

// Install installs the Shepherd daemon as a system user service.
func Install(execPath, workingDir string) (string, error) {
	if execPath == "" {
		p, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("detect executable: %w", err)
		}
		execPath, err = filepath.EvalSymlinks(p)
		if err != nil {
			execPath = p
		}
	}
	if workingDir == "" {
		var err error
		workingDir, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working dir: %w", err)
		}
	}

	switch runtime.GOOS {
	case "linux":
		unitPath, err := SystemdUnitPath()
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(unitPath), 0755); err != nil {
			return "", fmt.Errorf("create systemd dir: %w", err)
		}
		content := GenerateSystemdUnit(execPath, workingDir)
		if err := os.WriteFile(unitPath, []byte(content), 0644); err != nil {
			return "", fmt.Errorf("write systemd unit: %w", err)
		}

		// Try systemctl commands (ignore if systemctl is not available in mock/containers)
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		_ = exec.Command("systemctl", "--user", "enable", "shepherd.service").Run()

		return fmt.Sprintf("installed systemd user service: %s\nRun 'systemctl --user start shepherd.service' to start.", unitPath), nil

	case "darwin":
		plistPath, err := LaunchdPlistPath()
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(plistPath), 0755); err != nil {
			return "", fmt.Errorf("create launchd dir: %w", err)
		}
		logDir := filepath.Dir(plistPath)
		content := GenerateLaunchdPlist(execPath, workingDir, logDir)
		if err := os.WriteFile(plistPath, []byte(content), 0644); err != nil {
			return "", fmt.Errorf("write launchd plist: %w", err)
		}

		_ = exec.Command("launchctl", "load", plistPath).Run()
		return fmt.Sprintf("installed launchd user agent: %s", plistPath), nil

	default:
		return "", fmt.Errorf("service installation not supported on %s", runtime.GOOS)
	}
}

// Uninstall uninstalls the Shepherd daemon user service.
func Uninstall() (string, error) {
	switch runtime.GOOS {
	case "linux":
		unitPath, err := SystemdUnitPath()
		if err != nil {
			return "", err
		}
		_ = exec.Command("systemctl", "--user", "stop", "shepherd.service").Run()
		_ = exec.Command("systemctl", "--user", "disable", "shepherd.service").Run()
		if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("remove unit: %w", err)
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		return fmt.Sprintf("uninstalled systemd user service from %s", unitPath), nil

	case "darwin":
		plistPath, err := LaunchdPlistPath()
		if err != nil {
			return "", err
		}
		_ = exec.Command("launchctl", "unload", plistPath).Run()
		if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("remove plist: %w", err)
		}
		return fmt.Sprintf("uninstalled launchd user agent from %s", plistPath), nil

	default:
		return "", fmt.Errorf("service uninstallation not supported on %s", runtime.GOOS)
	}
}

// Status checks the status of the Shepherd daemon user service.
func Status() (string, error) {
	switch runtime.GOOS {
	case "linux":
		cmd := exec.Command("systemctl", "--user", "status", "shepherd.service")
		out, _ := cmd.CombinedOutput()
		return string(bytes.TrimSpace(out)), nil

	case "darwin":
		cmd := exec.Command("launchctl", "list")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("launchctl list: %w", err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "ai.shepherd.daemon") {
				return line, nil
			}
		}
		return "ai.shepherd.daemon is not running", nil

	default:
		return "", fmt.Errorf("service status not supported on %s", runtime.GOOS)
	}
}
