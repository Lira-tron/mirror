package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

var (
	overallProgress int64
	overallSize     int64
	skipped         int
	copied          int
)

// silentWriter tracks progress without printing
type silentWriter struct {
	total int64
}

func (s *silentWriter) Write(p []byte) (n int, err error) {
	atomic.AddInt64(&overallProgress, int64(len(p)))
	return len(p), nil
}

// progressWriter tracks and displays progress for a file
type progressWriter struct {
	fileName   string
	total      int64
	current    int64
	lastUpdate time.Time
}

func (w *progressWriter) Write(p []byte) (n int, err error) {
	n = len(p)
	atomic.AddInt64(&w.current, int64(n))
	atomic.AddInt64(&overallProgress, int64(n))
	
	// Throttle updates to avoid excessive output
	now := time.Now()
	if now.Sub(w.lastUpdate) < 65*time.Millisecond {
		return n, nil
	}
	w.lastUpdate = now
	
	current := atomic.LoadInt64(&w.current)
	pct := (current * 100) / w.total
	if pct > 100 {
		pct = 100
	}
	
	// Create progress bar
	barWidth := 40
	filledWidth := int(pct * int64(barWidth) / 100)
	bar := "[" + strings.Repeat("█", filledWidth) + strings.Repeat(" ", barWidth-filledWidth) + "]"
	
	// Calculate speed
	speed := float64(current) / 1024 / 1024 // MB
	speedStr := fmt.Sprintf("%.1f MB/s", speed)
	
	output := fmt.Sprintf("%s %3d%% %s (%s)", w.fileName, pct, bar, speedStr)
	
	// Use carriage return + clear line to ensure single line output
	fmt.Fprintf(os.Stderr, "\r%s", output)
	
	return n, nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <source> <target>\n", os.Args[0])
		os.Exit(1)
	}

	srcRoot := filepath.Clean(os.Args[1])
	dstRoot := filepath.Clean(os.Args[2])

	// First pass: calculate total size
	filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink == 0 {
			if info, err := d.Info(); err == nil {
				overallSize += info.Size()
			}
		}
		return nil
	})

	// Second pass: copy files
	err := filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		dstPath := filepath.Join(dstRoot, rel)

		// Skip if destination already exists
		if _, err := os.Stat(dstPath); err == nil {
			if !d.IsDir() {
				fmt.Printf("[SKIP] %s\n", rel)
				skipped++
			}
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}

		// Handle directories
		if d.IsDir() {
			return os.MkdirAll(dstPath, 0o755)
		}

		// Skip symlinks
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}

		copied++
		return copyFile(path, dstPath)
	})

	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}

	fmt.Printf("Mirror complete: %d copied, %d skipped\n", copied, skipped)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	fileName := filepath.Base(src)
	progressWriter := &progressWriter{
		fileName: fileName,
		total:    info.Size(),
	}

	// Use TeeReader to update progress and copy file
	reader := io.TeeReader(in, progressWriter)
	_, err = io.Copy(out, reader)
	fmt.Fprint(os.Stderr, "\n")
	
	// Display overall progress after each file copy
	if overallSize > 0 {
		pct := (atomic.LoadInt64(&overallProgress) * 100) / overallSize
		fmt.Printf("Overall: %d%% completed\n", pct)
	}
	
	return err
}
