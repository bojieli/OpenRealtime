package campaign

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/internal/fileidentity"
)

const maximumAggregateFileBytes = int64(maximumAggregateJSON)

func aggregatePublicationLeasePath(receiptPath string) string {
	return receiptPath + ".publication.lock"
}

func acquireAggregatePublicationLease(ctx context.Context, receiptPath string) (*os.File, error) {
	if ctx == nil {
		return nil, errors.New("acquire candidate review aggregate publication lease: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := aggregatePublicationLeasePath(receiptPath)
	parentPath, name := filepath.Dir(path), filepath.Base(path)
	parent, parentIdentity, err := openAggregateParent(parentPath)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	info, err := parent.Lstat(name)
	if os.IsNotExist(err) {
		created, createErr := parent.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			return nil, errors.New("create candidate review aggregate publication lease marker")
		}
		if syncErr := created.Sync(); syncErr != nil {
			_ = created.Close()
			return nil, errors.New("sync candidate review aggregate publication lease marker")
		}
		if closeErr := created.Close(); closeErr != nil {
			return nil, errors.New("close candidate review aggregate publication lease marker")
		}
		if syncErr := syncAggregateDirectory(parent, "."); syncErr != nil {
			return nil, syncErr
		}
		info, err = parent.Lstat(name)
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("candidate review aggregate publication lease marker is invalid")
	}
	lease, err := parent.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return nil, errors.New("open candidate review aggregate publication lease marker")
	}
	opened, statErr := lease.Stat()
	if statErr != nil || !os.SameFile(info, opened) || fileidentity.RequireSingleLink(lease) != nil {
		_ = lease.Close()
		return nil, errors.New("candidate review aggregate publication lease marker changed while opening")
	}
	if err := lockAggregatePublicationFile(lease); err != nil {
		_ = lease.Close()
		return nil, errors.New("candidate review aggregate publication is already active")
	}
	if err := ctx.Err(); err != nil {
		_ = lease.Close()
		return nil, err
	}
	after, statErr := parent.Lstat(name)
	visibleParent, parentErr := os.Lstat(parentPath)
	if statErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() ||
		!os.SameFile(opened, after) || fileidentity.RequireSingleLink(lease) != nil ||
		parentErr != nil || visibleParent.Mode()&os.ModeSymlink != 0 || !visibleParent.IsDir() ||
		!os.SameFile(parentIdentity, visibleParent) {
		_ = lease.Close()
		return nil, errors.New("candidate review aggregate publication lease changed after locking")
	}
	return lease, nil
}

func closeAggregatePublicationLease(lease *os.File) error {
	if lease == nil {
		return nil
	}
	if err := lease.Close(); err != nil {
		return errors.New("close candidate review aggregate publication lease")
	}
	return nil
}

func aggregateStageDirectory(final string) string {
	return filepath.Join(
		filepath.Dir(final), ".openrealtime-candidate-campaign-stage-"+digestName(filepath.Base(final))[:24],
	)
}

func openAggregateDirectory(path string) (*os.Root, os.FileInfo, error) {
	visible, err := os.Lstat(path)
	if err != nil || visible.Mode()&os.ModeSymlink != 0 || !visible.IsDir() ||
		visible.Mode().Perm()&0o077 != 0 {
		return nil, nil, errors.New("candidate review aggregate directory is invalid")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, errors.New("open candidate review aggregate directory")
	}
	opened, openErr := root.Stat(".")
	after, visibleErr := os.Lstat(path)
	if openErr != nil || visibleErr != nil || after.Mode()&os.ModeSymlink != 0 ||
		!after.IsDir() || !os.SameFile(visible, opened) || !os.SameFile(opened, after) {
		_ = root.Close()
		return nil, nil, errors.New("candidate review aggregate directory changed while opening")
	}
	return root, opened, nil
}

func createAggregateDirectory(path string) (*os.Root, os.FileInfo, error) {
	parentPath, name := filepath.Dir(path), filepath.Base(path)
	parent, parentIdentity, err := openAggregateParent(parentPath)
	if err != nil {
		return nil, nil, err
	}
	defer parent.Close()
	if err := parent.Mkdir(name, 0o700); err != nil {
		return nil, nil, errors.New("create candidate review aggregate directory exclusively")
	}
	if err := syncAggregateDirectory(parent, "."); err != nil {
		return nil, nil, err
	}
	created, err := parent.Lstat(name)
	if err != nil || created.Mode()&os.ModeSymlink != 0 || !created.IsDir() {
		return nil, nil, errors.New("inspect created candidate review aggregate directory")
	}
	visibleParent, err := os.Lstat(parentPath)
	if err != nil || !os.SameFile(parentIdentity, visibleParent) {
		return nil, nil, errors.New("candidate review aggregate parent changed during creation")
	}
	root, identity, err := openAggregateDirectory(path)
	if err != nil || !os.SameFile(created, identity) {
		if root != nil {
			_ = root.Close()
		}
		return nil, nil, errors.New("anchor created candidate review aggregate directory")
	}
	return root, identity, nil
}

func openAggregateParent(path string) (*os.Root, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, nil, errors.New("candidate review aggregate parent is invalid")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, errors.New("open candidate review aggregate parent")
	}
	opened, openErr := root.Stat(".")
	visible, visibleErr := os.Lstat(path)
	if openErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.IsDir() || !os.SameFile(before, opened) || !os.SameFile(opened, visible) {
		_ = root.Close()
		return nil, nil, errors.New("candidate review aggregate parent changed while opening")
	}
	return root, opened, nil
}

func aggregateWriteExclusive(
	root *os.Root, name string, payload []byte, purpose string,
) (AggregateFile, error) {
	if root == nil || len(payload) == 0 || int64(len(payload)) > maximumAggregateFileBytes ||
		!safeAggregateRelative(name) || purpose == "" {
		return AggregateFile{}, errors.New("candidate review aggregate file is invalid")
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return AggregateFile{}, errors.New("create candidate review aggregate file exclusively")
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if _, err := file.Write(payload); err != nil {
		return AggregateFile{}, errors.New("write candidate review aggregate file")
	}
	if err := file.Sync(); err != nil {
		return AggregateFile{}, errors.New("sync candidate review aggregate file")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(payload)) ||
		fileidentity.RequireSingleLink(file) != nil {
		return AggregateFile{}, errors.New("candidate review aggregate file identity is invalid")
	}
	if err := file.Close(); err != nil {
		return AggregateFile{}, errors.New("close candidate review aggregate file")
	}
	closed = true
	if err := syncAggregateDirectory(root, filepath.ToSlash(filepath.Dir(name))); err != nil {
		return AggregateFile{}, err
	}
	return AggregateFile{
		Path: filepath.ToSlash(name), Purpose: purpose,
		SHA256: aggregateDigest(payload), SizeBytes: int64(len(payload)),
	}, nil
}

func syncAggregateDirectory(root *os.Root, name string) error {
	directory, err := root.Open(name)
	if err != nil {
		return errors.New("open candidate review aggregate directory for sync")
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return errors.New("sync candidate review aggregate directory")
	}
	return nil
}

func aggregateReadRegular(root *os.Root, name string, expected int64) ([]byte, error) {
	if root == nil || !safeAggregateRelative(name) || expected <= 0 ||
		expected > maximumAggregateFileBytes {
		return nil, errors.New("candidate review aggregate read is invalid")
	}
	before, err := root.Lstat(name)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Mode().Perm()&0o022 != 0 || before.Size() != expected {
		return nil, errors.New("candidate review aggregate file is not a bounded regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, errors.New("open candidate review aggregate file")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || fileidentity.RequireSingleLink(file) != nil {
		return nil, errors.New("candidate review aggregate file changed while opening")
	}
	payload, err := io.ReadAll(io.LimitReader(file, expected+1))
	if err != nil || int64(len(payload)) != expected {
		return nil, errors.New("read exact candidate review aggregate file")
	}
	after, statErr := file.Stat()
	visible, visibleErr := root.Lstat(name)
	if statErr != nil || visibleErr != nil || visible.Mode()&os.ModeSymlink != 0 ||
		!visible.Mode().IsRegular() || !os.SameFile(opened, after) ||
		!os.SameFile(after, visible) || after.Size() != expected ||
		fileidentity.RequireSingleLink(file) != nil {
		return nil, errors.New("candidate review aggregate file changed while reading")
	}
	return payload, nil
}

func walkAggregateFiles(root *os.Root) ([]AggregateFile, error) {
	files := make([]AggregateFile, 0, 3)
	err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("walk candidate review aggregate")
		}
		if path == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
			info.Size() <= 0 || info.Size() > maximumAggregateFileBytes {
			return errors.New("candidate review aggregate contains an invalid entry")
		}
		payload, err := aggregateReadRegular(root, filepath.ToSlash(path), info.Size())
		if err != nil {
			return err
		}
		purpose := ""
		switch filepath.ToSlash(path) {
		case aggregateResultName:
			purpose = "candidate campaign result"
		case aggregateReviewName:
			purpose = "case-by-case human review"
		case aggregateManifestName:
			purpose = "candidate campaign commit marker"
		default:
			return errors.New("candidate review aggregate contains an unexpected file")
		}
		files = append(files, AggregateFile{
			Path: filepath.ToSlash(path), Purpose: purpose,
			SHA256: aggregateDigest(payload), SizeBytes: int64(len(payload)),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	return files, nil
}

func safeAggregateRelative(path string) bool {
	return path != "" && len(path) <= 4096 && !filepath.IsAbs(path) &&
		filepath.Clean(path) == path && path != "." && path != ".." &&
		!strings.Contains(path, `\`) && !strings.HasPrefix(path, ".."+string(filepath.Separator))
}

func decodeAggregateCanonical(payload []byte, destination any) error {
	if len(payload) == 0 || len(payload) > maximumAggregateJSON || validateAggregateJSON(payload) != nil {
		return errors.New("candidate review aggregate JSON is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("decode candidate review aggregate JSON")
	}
	canonical, err := aggregateCanonical(destination)
	if err != nil || !bytes.Equal(payload, canonical) {
		return errors.New("candidate review aggregate JSON is noncanonical")
	}
	return nil
}

func writeAggregateExternal(path string, payload []byte) error {
	parent, _, err := openAggregateParent(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	if _, err := aggregateWriteExclusive(
		parent, filepath.Base(path), payload, "candidate campaign external receipt",
	); err != nil {
		return errors.New("retain candidate review aggregate receipt")
	}
	return nil
}

func readAggregateExternal(path string) ([]byte, error) {
	parent, _, err := openAggregateParent(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	info, err := parent.Lstat(filepath.Base(path))
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() <= 0 || info.Size() > maximumAggregateJSON {
		return nil, errors.New("candidate review aggregate receipt is invalid")
	}
	return aggregateReadRegular(parent, filepath.Base(path), info.Size())
}
