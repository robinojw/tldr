// Package backup manages harness config backups for safe rollback.
package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/robinojw/tldr/internal/logging"
	"github.com/robinojw/tldr/pkg/config"
)

var log = logging.New("backup")

// Backup creates a timestamped backup of a file.
func Backup(filePath string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read file for backup: %w", err)
	}

	dir := config.BackupDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create backup directory: %w", err)
	}

	base := filepath.Base(filePath)
	ts := time.Now().Format("20060102-150405")
	backupName := fmt.Sprintf("%s.%s.bak", base, ts)
	backupPath := filepath.Join(dir, backupName)

	if err := os.WriteFile(backupPath, data, 0644); err != nil {
		return "", fmt.Errorf("failed to write backup: %w", err)
	}

	log.Info("backed up %s -> %s", filePath, backupPath)
	return backupPath, nil
}

// Restore copies the latest backup back to the original path.
func Restore(filePath string) error {
	dir := config.BackupDir()
	base := filepath.Base(filePath)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("no backups found: %w", err)
	}

	// Find the latest backup for this file
	var latest string
	for _, e := range entries {
		if !e.IsDir() && matchBackup(e.Name(), base) {
			latest = e.Name()
		}
	}

	if latest == "" {
		return fmt.Errorf("no backup found for %s", base)
	}

	backupPath := filepath.Join(dir, latest)
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("failed to read backup %s: %w", backupPath, err)
	}

	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return fmt.Errorf("failed to restore %s: %w", filePath, err)
	}

	log.Info("restored %s from %s", filePath, backupPath)
	return nil
}

// ListBackups returns all backup files.
func ListBackups() ([]string, error) {
	dir := config.BackupDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var backups []string
	for _, e := range entries {
		if !e.IsDir() {
			backups = append(backups, filepath.Join(dir, e.Name()))
		}
	}
	return backups, nil
}

func matchBackup(fileName, originalBase string) bool {
	// Backup names are like "file.ext.20060102-150405.bak"
	return len(fileName) > len(originalBase) &&
		fileName[:len(originalBase)] == originalBase
}
