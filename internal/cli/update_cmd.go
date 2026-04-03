package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/robinojw/tldr/internal/logging"
	"github.com/robinojw/tldr/pkg/config"
	"github.com/spf13/cobra"
)

var updateLog = logging.New("cli:update")

func newUpdateCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update tldr to the latest version",
		Long: `Check for a newer version of tldr and install it. Checks GitHub releases
first; if no releases exist, builds from the latest source.

The current binary is replaced in-place. The previous version is kept as
a .bak file next to the binary in case you need to roll back.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(force)
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "Force update even if already on latest version")
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the current tldr version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("tldr %s\n", config.Version)
		},
	}
}

const (
	repo           = "robinojw/tldr"
	releasesAPIURL = "https://api.github.com/repos/" + repo + "/releases/latest"
	commitsAPIURL  = "https://api.github.com/repos/" + repo + "/commits/main"
)

type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

type githubCommit struct {
	SHA string `json:"sha"`
}

func runUpdate(force bool) error {
	currentBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine current binary path: %w", err)
	}
	currentBin, err = filepath.EvalSymlinks(currentBin)
	if err != nil {
		return fmt.Errorf("cannot resolve binary path: %w", err)
	}

	fmt.Printf("Current version: %s\n", config.Version)
	fmt.Printf("Binary: %s\n", currentBin)
	fmt.Println()

	// Try release-based update first
	updated, err := tryReleaseUpdate(currentBin, force)
	if err != nil {
		updateLog.Warn("release check failed: %v", err)
	}
	if updated {
		return nil
	}

	// Fall back to building from source
	return buildFromSource(currentBin, force)
}

func tryReleaseUpdate(currentBin string, force bool) (bool, error) {
	fmt.Println("Checking for releases...")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(releasesAPIURL)
	if err != nil {
		return false, fmt.Errorf("failed to check releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		fmt.Println("No releases found.")
		return false, nil
	}
	if resp.StatusCode != 200 {
		return false, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return false, fmt.Errorf("failed to parse release: %w", err)
	}

	latestVersion := strings.TrimPrefix(release.TagName, "v")
	currentVersion := strings.TrimPrefix(config.Version, "v")

	fmt.Printf("Latest release: %s\n", release.TagName)

	if !force && latestVersion == currentVersion {
		fmt.Println("Already on the latest release.")
		return true, nil
	}

	// Find matching asset
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	assetName := fmt.Sprintf("tldr_%s_%s_%s.tar.gz", latestVersion, goos, goarch)

	var downloadURL string
	for _, asset := range release.Assets {
		if asset.Name == assetName {
			downloadURL = asset.BrowserDownloadURL
			break
		}
	}

	if downloadURL == "" {
		fmt.Printf("No binary for %s/%s in release. Falling back to source build.\n", goos, goarch)
		return false, nil
	}

	fmt.Printf("Downloading %s...\n", assetName)

	tmpDir, err := os.MkdirTemp("", "tldr-update-*")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmpDir)

	tarPath := filepath.Join(tmpDir, assetName)
	if err := downloadFile(downloadURL, tarPath); err != nil {
		return false, fmt.Errorf("download failed: %w", err)
	}

	// Extract
	extractCmd := exec.Command("tar", "-xzf", tarPath, "-C", tmpDir)
	if out, err := extractCmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("extract failed: %s: %w", string(out), err)
	}

	newBin := filepath.Join(tmpDir, "tldr")
	if _, err := os.Stat(newBin); err != nil {
		return false, fmt.Errorf("binary not found in archive")
	}

	if err := replaceBinary(currentBin, newBin); err != nil {
		return false, err
	}

	fmt.Printf("Updated to %s\n", release.TagName)
	return true, nil
}

func buildFromSource(currentBin string, force bool) error {
	// Check if Go is available
	goPath, err := exec.LookPath("go")
	if err != nil {
		return fmt.Errorf("Go is required to build from source. Install Go 1.22+ from https://go.dev/dl/")
	}

	fmt.Println("Building from latest source...")

	// Check latest commit
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(commitsAPIURL)
	if err != nil {
		updateLog.Warn("could not check latest commit: %v", err)
	} else {
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			var commit githubCommit
			if err := json.NewDecoder(resp.Body).Decode(&commit); err == nil {
				shortSHA := commit.SHA
				if len(shortSHA) > 7 {
					shortSHA = shortSHA[:7]
				}
				fmt.Printf("Latest commit: %s\n", shortSHA)

				// If current version contains this SHA, we're up to date
				if !force && strings.Contains(config.Version, shortSHA) {
					fmt.Println("Already on the latest commit.")
					return nil
				}
			}
		}
	}

	tmpDir, err := os.MkdirTemp("", "tldr-update-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	// Check for git
	gitPath, _ := exec.LookPath("git")
	newBin := filepath.Join(tmpDir, "tldr")

	if gitPath != "" {
		fmt.Println("Cloning repository...")
		cloneCmd := exec.Command(gitPath, "clone", "--depth", "1",
			"https://github.com/"+repo+".git", filepath.Join(tmpDir, "src"))
		cloneCmd.Stderr = os.Stderr
		if err := cloneCmd.Run(); err != nil {
			return fmt.Errorf("git clone failed: %w", err)
		}

		// Get the commit SHA for version string
		revCmd := exec.Command(gitPath, "rev-parse", "--short", "HEAD")
		revCmd.Dir = filepath.Join(tmpDir, "src")
		revOut, _ := revCmd.Output()
		commitSHA := strings.TrimSpace(string(revOut))

		ldflags := fmt.Sprintf("-s -w -X github.com/robinojw/tldr/pkg/config.Version=dev-%s", commitSHA)

		fmt.Println("Building...")
		buildCmd := exec.Command(goPath, "build", "-ldflags", ldflags, "-o", newBin, "./cmd/tldr")
		buildCmd.Dir = filepath.Join(tmpDir, "src")
		buildCmd.Stderr = os.Stderr
		if err := buildCmd.Run(); err != nil {
			return fmt.Errorf("build failed: %w", err)
		}
	} else {
		fmt.Println("Installing via go install...")
		installCmd := exec.Command(goPath, "install", "github.com/"+repo+"/cmd/tldr@latest")
		installCmd.Env = append(os.Environ(), "GOBIN="+tmpDir)
		installCmd.Stderr = os.Stderr
		if err := installCmd.Run(); err != nil {
			return fmt.Errorf("go install failed: %w", err)
		}
	}

	if _, err := os.Stat(newBin); err != nil {
		return fmt.Errorf("build produced no binary")
	}

	if err := replaceBinary(currentBin, newBin); err != nil {
		return err
	}

	fmt.Println("Updated to latest source build.")
	return nil
}

func replaceBinary(currentBin, newBin string) error {
	// Back up the current binary
	backupPath := currentBin + ".bak"
	if err := copyFile(currentBin, backupPath); err != nil {
		updateLog.Warn("could not create backup at %s: %v", backupPath, err)
		// Continue anyway -- the update is more important
	} else {
		fmt.Printf("Backup: %s\n", backupPath)
	}

	// Make new binary executable
	if err := os.Chmod(newBin, 0755); err != nil {
		return fmt.Errorf("chmod failed: %w", err)
	}

	// Try direct rename first (works if same filesystem)
	if err := os.Rename(newBin, currentBin); err != nil {
		// Cross-filesystem: copy then remove
		if err := copyFile(newBin, currentBin); err != nil {
			// Try with sudo
			fmt.Println("Writing requires elevated permissions...")
			sudoCmd := exec.Command("sudo", "cp", newBin, currentBin)
			sudoCmd.Stdin = os.Stdin
			sudoCmd.Stdout = os.Stdout
			sudoCmd.Stderr = os.Stderr
			if err := sudoCmd.Run(); err != nil {
				return fmt.Errorf("failed to install binary: %w", err)
			}
		}
	}

	return nil
}

func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	// Preserve permissions
	info, err := os.Stat(src)
	if err == nil {
		_ = os.Chmod(dst, info.Mode())
	}

	return nil
}
