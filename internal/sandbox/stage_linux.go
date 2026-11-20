package sandbox

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type stageBudget struct {
	files, dirs int
	bytes       int64
}

var defaultStageBudget = stageBudget{files: 10_000, dirs: 10_000, bytes: 256 << 20}

const maxStagedFile = 32 << 20

func stage(ctx context.Context, source Source, dest string, budget stageBudget) error {
	dirs := map[string]bool{".": true}
	var dirsNeeded func(string) error
	dirsNeeded = func(dir string) error {
		if dirs[dir] {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := dirsNeeded(path.Dir(dir)); err != nil {
			return err
		}
		if budget.dirs == 0 {
			return fmt.Errorf("%w: staged directory count", ErrLimit)
		}
		if err := os.Mkdir(filepath.Join(dest, dir), 0o700); err != nil {
			return err
		}
		budget.dirs--
		dirs[dir] = true
		return nil
	}

	err := source.Walk(ctx, func(name string, info fs.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if info == nil {
			return fmt.Errorf("source returned missing file information")
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if name == "." || !fs.ValidPath(name) || strings.ContainsRune(name, 0) {
			return fmt.Errorf("source returned an invalid relative file path")
		}
		fileBudget := min(budget.bytes, maxStagedFile)
		if len(name) > 4095 || budget.files == 0 || info.Size() > fileBudget {
			return fmt.Errorf("%w: staged path length, file count or byte budget", ErrLimit)
		}

		// A file can grow after Walk. Ask Source for one extra byte so
		// both bounded reads and Sources that reject overflow fail closed.
		data, err := source.Read(name, fileBudget+1)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return fmt.Errorf("read staged file %q: %w", name, err)
		}
		if int64(len(data)) > fileBudget {
			return fmt.Errorf("%w: staged byte budget", ErrLimit)
		}
		if err := dirsNeeded(path.Dir(name)); err != nil {
			return err
		}
		if err := writeCopy(filepath.Join(dest, name), data); err != nil {
			return err
		}
		budget.files--
		budget.bytes -= int64(len(data))
		return ctx.Err()
	})
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func writeCopy(name string, data []byte) error {
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
