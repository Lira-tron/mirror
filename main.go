package main

import (
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

var (
	overallProgress int64
	overallSize     int64
	skipped         int
	copied          int
	startTime       time.Time
	copyFlag        bool
	moveFlag        bool
	applyFlag       bool
	sourceFlag      string
	targetFlag      string
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

	// Create animated progress bar with moving effect
	barWidth := 40
	filledWidth := int(pct * int64(barWidth) / 100)
	if filledWidth > barWidth {
		filledWidth = barWidth
	}

	// Animation frames for marching ants effect
	frames := []string{"▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}
	animFrame := frames[int(now.Unix()*4)%len(frames)]

	emptyWidth := barWidth - filledWidth - 1
	if emptyWidth < 0 {
		emptyWidth = 0
	}
	bar := "[" + strings.Repeat("█", filledWidth) + animFrame + strings.Repeat(" ", emptyWidth) + "]"

	// Calculate speed
	speed := float64(current) / 1024 / 1024 // MB
	speedStr := fmt.Sprintf("%.1f MB/s", speed)

	output := fmt.Sprintf("%s %3d%% %s (%s)", w.fileName, pct, bar, speedStr)

	// Use carriage return + clear line to ensure single line output
	fmt.Fprintf(os.Stderr, "\r%s", output)

	return n, nil
}

// cleanFilename removes numbered variants like (1), (2), (123), (1) with spaces, etc.
func cleanFilename(filename string) string {
	// Match patterns like " (1)", " (2)", "(1)", "(123)", etc.
	re := regexp.MustCompile(` ?\(\d+\)`)
	return re.ReplaceAllString(filename, "")
}

func main() {
	flag.BoolVar(&copyFlag, "copy", false, "copy files from source to target")
	flag.BoolVar(&moveFlag, "move", false, "move files from source to target")
	flag.BoolVar(&applyFlag, "apply", false, "apply the copy/move operation (without this flag, only lists files)")
	flag.StringVar(&sourceFlag, "source", "", "source directory")
	flag.StringVar(&targetFlag, "target", "", "target directory")
	
	// Tool flags
	duplicatesFlag := flag.Bool("duplicates", false, "find duplicate files with (1) in name")
	xmpFlag := flag.Bool("xmp", false, "rename XMP sidecar files to match their image files")
	
	flag.Parse()
	startTime = time.Now()

	// Determine which operation to run
	if *duplicatesFlag || *xmpFlag {
		// Tool operations (duplicates or xmp)
		runToolOperation(*duplicatesFlag, *xmpFlag, targetFlag, applyFlag)
	} else if copyFlag || moveFlag {
		// Copy/move operations
		runCopyMoveOperation()
	} else {
		fmt.Fprintf(os.Stderr, "Usage: %s (--copy | --move) --source <source> --target <target> [--apply]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "   or: %s (--duplicates | --xmp) --target <directory> [--apply]\n", os.Args[0])
		os.Exit(1)
	}
}

func runToolOperation(duplicates, xmp bool, dir string, apply bool) {
	if duplicates && xmp {
		fmt.Fprintf(os.Stderr, "Error: cannot specify both --duplicates and --xmp\n")
		os.Exit(1)
	}

	if dir == "" {
		fmt.Fprintf(os.Stderr, "Error: --dir flag is required\n")
		os.Exit(1)
	}

	if _, err := os.Stat(dir); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Directory '%s' not found\n", dir)
		os.Exit(1)
	}

	if xmp {
		handleXMPRenaming(dir, apply)
	} else {
		handleDuplicates(dir, apply)
	}
}

func runCopyMoveOperation() {
	// Validate flags
	if !copyFlag && !moveFlag {
		fmt.Fprintf(os.Stderr, "Usage: %s (--copy | --move) --source <source> --target <target> [--apply]\n", os.Args[0])
		os.Exit(1)
	}

	if copyFlag && moveFlag {
		fmt.Fprintf(os.Stderr, "Error: cannot specify both --copy and --move\n")
		os.Exit(1)
	}

	if sourceFlag == "" || targetFlag == "" {
		fmt.Fprintf(os.Stderr, "Error: --source and --target flags are required\n")
		os.Exit(1)
	}

	srcRoot := filepath.Clean(sourceFlag)
	dstRoot := filepath.Clean(targetFlag)

	if _, err := os.Stat(srcRoot); err != nil {
		fmt.Fprintf(os.Stderr, "Source does not exist: %s\n", srcRoot)
		os.Exit(1)
	}
	if _, err := os.Stat(dstRoot); err != nil {
		fmt.Fprintf(os.Stderr, "Target does not exist: %s\n", dstRoot)
		os.Exit(1)
	}

	// First pass: calculate total size
	fmt.Fprintf(os.Stderr, "Calculating total size...\n")
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
	fmt.Fprintf(os.Stderr, "Total size: %.2f MB\n", float64(overallSize)/1024/1024)

	// Second pass: list or apply copy/move
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
			if applyFlag {
				return os.MkdirAll(dstPath, 0o755)
			}
			return nil
		}

		// Skip symlinks
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}

		copied++
		if applyFlag {
			if moveFlag {
				return moveFile(path, dstPath, rel)
			}
			return copyFile(path, dstPath, rel)
		} else {
			// Just list the files to be copied/moved
			operation := "COPY"
			if moveFlag {
				operation = "MOVE"
			}
			fmt.Printf("[%s] %s\n", operation, rel)
		}
		return nil
	})

	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}

	operation := "copy"
	if moveFlag {
		operation = "move"
	}

	if applyFlag {
		fmt.Printf("Operation complete: %d files %sd, %d skipped\n", copied, operation, skipped)
	} else {
		fmt.Printf("Preview: %d files will be %sd\n", copied, operation)
	}
}

func moveFile(src, dst, relPath string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	info, err := os.Stat(src)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "[MOVE] %s\n", relPath)

	atomic.AddInt64(&overallProgress, info.Size())

	if err := os.Rename(src, dst); err != nil {
		return err
	}

	// Display overall progress with animated bar after each file move
	if overallSize > 0 {
		pct := (atomic.LoadInt64(&overallProgress) * 100) / overallSize
		if pct > 100 {
			pct = 100
		}
		fmt.Fprintf(os.Stderr, "\rOverall: %d%%\n", pct)
	}

	return nil
}

func copyFile(src, dst, relPath string) error {
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

	fmt.Fprintf(os.Stderr, "[COPY] %s\n", relPath)

	fileName := filepath.Base(src)
	progressWriter := &progressWriter{
		fileName: fileName,
		total:    info.Size(),
	}

	// Use TeeReader to update progress and copy file
	reader := io.TeeReader(in, progressWriter)
	_, err = io.Copy(out, reader)
	fmt.Fprint(os.Stderr, "\n")

	// Display overall progress with animated bar after each file copy
	if overallSize > 0 {
		pct := (atomic.LoadInt64(&overallProgress) * 100) / overallSize
		if pct > 100 {
			pct = 100
		}
		fmt.Fprintf(os.Stderr, "\rOverall: %d%%\n", pct)
	}

	return err
}

func handleDuplicates(dir string, apply bool) {
	foundChanges := 0
	
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		
		baseName := d.Name()
		
		// Skip XMP files
		if strings.HasSuffix(baseName, ".xmp") {
			return nil
		}
		
		// Look for files with (N) pattern where N is any number
		if !strings.Contains(baseName, "(") {
			return nil
		}
		
		// Extract original name by removing numbered variants like (1), (2), etc.
		originalName := cleanFilename(baseName)
		
		// If nothing changed, skip
		if originalName == baseName {
			return nil
		}
		
		originalPath := filepath.Join(filepath.Dir(path), originalName)
		
		// Check if original file exists
		if _, err := os.Stat(originalPath); err != nil {
			return nil
		}
		
		// Compare file sizes
		info1, _ := os.Stat(path)
		info2, _ := os.Stat(originalPath)
		
		if info1 != nil && info2 != nil && info1.Size() == info2.Size() {
			foundChanges++
			// Show relative paths
			relOriginal, _ := filepath.Rel(dir, originalPath)
			relDuplicate, _ := filepath.Rel(dir, path)
			fmt.Printf("%s <-> %s (Size: %d bytes)\n", relOriginal, relDuplicate, info1.Size())
			
			if apply {
				if err := os.Remove(path); err != nil {
					fmt.Fprintf(os.Stderr, "Error removing %s: %v\n", path, err)
				} else {
					fmt.Printf("  Removed: %s\n", path)
				}
			}
		}
		
		return nil
	})
	
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	
	if foundChanges == 0 {
		fmt.Println("No duplicate files found.")
	} else {
		fmt.Printf("\nFound %d duplicate pair(s).\n", foundChanges)
		if apply {
			fmt.Println("Removed all duplicates.")
		}
	}
}

func handleXMPRenaming(dir string, apply bool) {
	foundChanges := 0
	skipped := 0
	xmpFiles := make(map[string]string) // map of xmp path to correct path
	
	// Scan for XMP files and determine if they need renaming
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		
		baseName := d.Name()
		if !strings.HasSuffix(baseName, ".xmp") {
			return nil
		}
		
		dirPath := filepath.Dir(path)
		imageBase := strings.TrimSuffix(baseName, ".xmp")
		
		// Extract clean base name (without numbered variants like (1), (2), etc.)
		cleanBase := cleanFilename(imageBase)
		
		// Get the expected extension from the XMP filename
		imageExt := filepath.Ext(imageBase)
		
		// Get the base name without extension from cleanBase
		cleanBaseOnly := strings.TrimSuffix(cleanBase, filepath.Ext(cleanBase))
		
		// Check if the exact image corresponding to this XMP exists
		// e.g., if XMP is "a (6).mp4.xmp", check if "a (6).mp4" exists
		exactImagePath := filepath.Join(dirPath, imageBase)
		if _, err := os.Stat(exactImagePath); err == nil {
			// The exact image exists, ignore this XMP
			return nil
		}
		
		// Find the corresponding image file
		// Prefer variants when they exist, otherwise use the clean version
		var foundImage string
		var cleanImageExists bool
		var numberedImageExists bool
		entries, _ := os.ReadDir(dirPath)
		
		// First pass: check what images exist
		for _, entry := range entries {
			if entry.IsDir() || strings.HasSuffix(entry.Name(), ".xmp") {
				continue
			}
			
			if !strings.EqualFold(filepath.Ext(entry.Name()), imageExt) {
				continue
			}
			
			entryBase := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
			entryClean := cleanFilename(entryBase)
			
			if strings.EqualFold(entryClean, cleanBaseOnly) {
				if strings.Contains(entry.Name(), "(") {
					numberedImageExists = true
				} else {
					cleanImageExists = true
				}
			}
		}
		
		// If both clean and numbered variants exist, ignore the XMP
		if cleanImageExists && numberedImageExists {
			return nil
		}
		
		// Second pass: find the image file to use
		for _, entry := range entries {
			if entry.IsDir() || strings.HasSuffix(entry.Name(), ".xmp") {
				continue
			}
			
			if !strings.EqualFold(filepath.Ext(entry.Name()), imageExt) {
				continue
			}
			
			entryBase := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
			entryClean := cleanFilename(entryBase)
			
			if strings.EqualFold(entryClean, cleanBaseOnly) && strings.Contains(entry.Name(), "(") {
				foundImage = filepath.Join(dirPath, entry.Name())
				break
			}
		}
		
		// If no numbered variant found, look for the clean version
		if foundImage == "" {
			for _, entry := range entries {
				if entry.IsDir() || strings.HasSuffix(entry.Name(), ".xmp") {
					continue
				}
				
				if !strings.EqualFold(filepath.Ext(entry.Name()), imageExt) {
					continue
				}
				
				entryBase := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
				entryClean := cleanFilename(entryBase)
				
				if strings.EqualFold(entryClean, cleanBaseOnly) && !strings.Contains(entry.Name(), "(") {
					foundImage = filepath.Join(dirPath, entry.Name())
					break
				}
			}
		}
		
		// If no image found, this XMP is orphaned
		if foundImage == "" {
			foundChanges++
			relPath, _ := filepath.Rel(dir, path)
			fmt.Printf("[REMOVE] %s (orphaned XMP, no base file exists)\n", relPath)
			if apply {
				if err := os.Remove(path); err != nil {
					fmt.Fprintf(os.Stderr, "Error removing %s: %v\n", path, err)
				}
			}
			return nil
		}
		
		correctXMPName := filepath.Base(foundImage) + ".xmp"
		correctXMPPath := filepath.Join(dirPath, correctXMPName)
		
		// Check if the exact XMP base file exists
		// e.g., if XMP is "a (1).ARW.xmp", check if "a (1).ARW" exists
		if _, err := os.Stat(exactImagePath); err != nil {
			// The exact base file doesn't exist, this XMP is orphaned
			foundChanges++
			relPath, _ := filepath.Rel(dir, path)
			fmt.Printf("[REMOVE] %s (orphaned XMP, no base file exists)\n", relPath)
			if apply {
				if err := os.Remove(path); err != nil {
					fmt.Fprintf(os.Stderr, "Error removing %s: %v\n", path, err)
				}
			}
			return nil
		}
		
		// If the XMP file is not already named correctly
		if path != correctXMPPath {
			// Check if destination file already exists
			entries, _ := os.ReadDir(dirPath)
			var existingFile string
			for _, entry := range entries {
				if strings.EqualFold(entry.Name(), correctXMPName) {
					existingFile = entry.Name()
					break
				}
			}
			
			if existingFile != "" {
				// File exists (with possibly different case)
				// If the actual filename differs from expected, it's a real conflict
				if existingFile != correctXMPName {
					// Different file or case variation, just skip silently
					return nil
				}
				// Same filename (case-sensitive)
				// Check if the base image actually exists
				if _, err := os.Stat(foundImage); err != nil {
					// Base image doesn't exist, the destination XMP is orphaned, remove it
					if apply {
						if err := os.Remove(correctXMPPath); err != nil {
							fmt.Fprintf(os.Stderr, "Error removing %s: %v\n", correctXMPPath, err)
						} else {
							relDestPath, _ := filepath.Rel(dir, correctXMPPath)
							fmt.Printf("[REMOVE] %s (orphaned XMP, base doesn't exist)\n", relDestPath)
						}
					} else {
						foundChanges++
						relDestPath, _ := filepath.Rel(dir, correctXMPPath)
						fmt.Printf("[REMOVE] %s (orphaned XMP, base doesn't exist)\n", relDestPath)
					}
				} else {
					// Base image exists, just skip
					skipped++
					relPath, _ := filepath.Rel(dir, path)
					relDestPath, _ := filepath.Rel(dir, correctXMPPath)
					fmt.Printf("[SKIP] %s (destination already exists: %s)\n", relPath, relDestPath)
				}
				return nil
			}
			
			foundChanges++
			relPath, _ := filepath.Rel(dir, path)
			relCorrectPath, _ := filepath.Rel(dir, correctXMPPath)
			fmt.Printf("%s -> %s\n", relPath, relCorrectPath)
			xmpFiles[path] = correctXMPPath
		}
		
		return nil
	})
	
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	
	// Apply changes if requested
	if apply {
		for src, dst := range xmpFiles {
			if err := os.Rename(src, dst); err != nil {
				fmt.Fprintf(os.Stderr, "Error renaming %s: %v\n", src, err)
			} else {
				fmt.Printf("  Renamed: %s to %s\n", src, dst)
			}
		}
	}
	
	if foundChanges == 0 && skipped == 0 {
		fmt.Println("No XMP files need renaming.")
	} else {
		if foundChanges > 0 {
			fmt.Printf("\nFound %d XMP file(s) to rename.\n", foundChanges)
			if apply {
				fmt.Println("Renamed all XMP files to match their image files.")
			}
		}
		if skipped > 0 {
			fmt.Printf("Skipped %d XMP file(s) (destination already exists).\n", skipped)
		}
	}
}
