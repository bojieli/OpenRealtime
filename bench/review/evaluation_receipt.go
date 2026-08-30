package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/internal/fileidentity"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const maximumEvaluationReceiptBytes = 64 << 10

// WriteEvaluationBundleReceipt retains a verified portable receipt outside
// its evaluation directory. The destination is create-only and opened through
// an identity-checked parent handle; a receipt can therefore anchor a sibling
// evaluation without becoming mutable content inside that evaluation.
func WriteEvaluationBundleReceipt(
	ctx context.Context, path string, receipt EvaluationBundleReceipt,
) (resultErr error) {
	return writeEvaluationBundleReceipt(ctx, path, receipt, true)
}

func writeEvaluationBundleReceipt(
	ctx context.Context, path string, receipt EvaluationBundleReceipt,
	requireCommittedDirectory bool,
) (resultErr error) {
	if ctx == nil {
		return errors.New("write evaluation receipt: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateEvaluationBundleReceipt(receipt); err != nil {
		return err
	}
	if requireCommittedDirectory {
		if err := validateEvaluationBundleDirectory(receipt.Directory, true); err != nil {
			return errors.New("evaluation receipt does not name a committed bundle directory")
		}
	} else if !validEvaluationReceiptDirectory(receipt.Directory) {
		return errors.New("evaluation receipt does not name a canonical future bundle directory")
	}
	if err := validateExternalEvaluationReceiptPath(path, receipt.Directory, false); err != nil {
		return err
	}
	payload, err := marshalCanonicalIndented(receipt, maximumEvaluationReceiptBytes)
	if err != nil {
		return err
	}
	parentPath, name := filepath.Dir(path), filepath.Base(path)
	root, err := openValidatedRoot(parentPath, nil)
	if err != nil {
		return fmt.Errorf("open evaluation receipt parent: %w", err)
	}
	written := false
	defer func() {
		if !written {
			_ = root.Remove(name)
		}
		if closeErr := root.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, errors.New("close evaluation receipt parent"))
		}
	}()
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("create evaluation receipt exclusively")
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	if _, err := file.Write(payload); err != nil {
		return errors.New("write evaluation receipt")
	}
	if err := file.Sync(); err != nil {
		return errors.New("sync evaluation receipt")
	}
	if err := file.Close(); err != nil {
		return errors.New("close evaluation receipt")
	}
	closed = true
	retained, err := readExternalEvaluationReceipt(ctx, root, name)
	if err != nil || !bytes.Equal(retained, payload) {
		return errors.New("reopen evaluation receipt")
	}
	decoded, err := decodeEvaluationBundleReceipt(retained)
	if err != nil || !sameEvaluationBundleReceipt(decoded, receipt) {
		return errors.New("verify retained evaluation receipt")
	}
	if err := syncEvaluationBundleDirectory(root); err != nil {
		return err
	}
	parentInfo, err := root.Stat(".")
	visibleParent, visibleErr := os.Lstat(parentPath)
	if err != nil || visibleErr != nil || visibleParent.Mode()&os.ModeSymlink != 0 ||
		!visibleParent.IsDir() || !os.SameFile(parentInfo, visibleParent) {
		return errors.New("evaluation receipt parent changed during publication")
	}
	written = true
	return nil
}

// ReadEvaluationBundleReceipt strictly reopens one canonical external receipt.
// Directory is informational; VerifyEvaluationBundle accepts a caller-selected
// relocated byte-identical bundle while comparing the portable digest fields.
func ReadEvaluationBundleReceipt(
	ctx context.Context, path string,
) (receipt EvaluationBundleReceipt, resultErr error) {
	if ctx == nil {
		return EvaluationBundleReceipt{}, errors.New("read evaluation receipt: nil context")
	}
	if err := ctx.Err(); err != nil {
		return EvaluationBundleReceipt{}, err
	}
	if err := validateExternalEvaluationReceiptPath(path, "", true); err != nil {
		return EvaluationBundleReceipt{}, err
	}
	parentPath, name := filepath.Dir(path), filepath.Base(path)
	root, err := openValidatedRoot(parentPath, nil)
	if err != nil {
		return EvaluationBundleReceipt{}, err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			receipt = EvaluationBundleReceipt{}
			resultErr = errors.Join(resultErr, errors.New("close evaluation receipt parent"))
		}
	}()
	payload, err := readExternalEvaluationReceipt(ctx, root, name)
	if err != nil {
		return EvaluationBundleReceipt{}, err
	}
	return decodeEvaluationBundleReceipt(payload)
}

func readExternalEvaluationReceipt(
	ctx context.Context, root *os.Root, name string,
) ([]byte, error) {
	file, info, err := openRegularNoSymlink(root, name, nil)
	if err != nil {
		return nil, errors.New("open evaluation receipt")
	}
	defer file.Close()
	if info.Size() <= 0 || info.Size() > maximumEvaluationReceiptBytes || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("evaluation receipt is empty, oversized, or overly permissive")
	}
	if err := fileidentity.RequireSingleLink(file); err != nil {
		return nil, errors.New("evaluation receipt is not exclusively retained")
	}
	payload, err := readBoundedContext(ctx, file, maximumEvaluationReceiptBytes)
	if err != nil || int64(len(payload)) != info.Size() {
		return nil, errors.New("read exact evaluation receipt")
	}
	after, err := root.Lstat(name)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() ||
		!os.SameFile(info, after) || after.Size() != info.Size() ||
		fileidentity.RequireSingleLink(file) != nil {
		return nil, errors.New("evaluation receipt changed while reading")
	}
	return payload, nil
}

func decodeEvaluationBundleReceipt(payload []byte) (EvaluationBundleReceipt, error) {
	if len(payload) == 0 || len(payload) > maximumEvaluationReceiptBytes ||
		strictjson.Validate(payload) != nil {
		return EvaluationBundleReceipt{}, errors.New("evaluation receipt is invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var receipt EvaluationBundleReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return EvaluationBundleReceipt{}, errors.New("decode evaluation receipt")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return EvaluationBundleReceipt{}, errors.New("evaluation receipt has trailing data")
	}
	canonical, err := marshalCanonicalIndented(receipt, maximumEvaluationReceiptBytes)
	if err != nil || !bytes.Equal(canonical, payload) {
		return EvaluationBundleReceipt{}, errors.New("evaluation receipt is noncanonical")
	}
	if err := validateEvaluationBundleReceipt(receipt); err != nil {
		return EvaluationBundleReceipt{}, err
	}
	if !validEvaluationReceiptDirectory(receipt.Directory) {
		return EvaluationBundleReceipt{}, errors.New("evaluation receipt directory is invalid")
	}
	return receipt, nil
}

func validateExternalEvaluationReceiptPath(path, bundleDirectory string, mustExist bool) error {
	if len(path) == 0 || len(path) > maximumRootDirectoryBytes || !utf8.ValidString(path) ||
		containsControl(path) || !filepath.IsAbs(path) || filepath.Clean(path) != path ||
		path == filepath.Dir(path) || filepath.Base(path) == "." || filepath.Base(path) == ".." {
		return errors.New("evaluation receipt path must be bounded, clean, absolute, and non-root")
	}
	if bundleDirectory != "" {
		if !validEvaluationReceiptDirectory(bundleDirectory) {
			return errors.New("evaluation receipt bundle directory is invalid")
		}
		if path == bundleDirectory || strings.HasPrefix(path, bundleDirectory+string(filepath.Separator)) {
			return errors.New("evaluation receipt must be outside its evaluation directory")
		}
	}
	parent := filepath.Dir(path)
	for current := parent; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("evaluation receipt parent must be a non-symlink directory")
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	info, err := os.Lstat(path)
	if mustExist {
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("evaluation receipt must be a regular non-symlink file")
		}
		return nil
	}
	if err == nil {
		return errors.New("evaluation receipt destination already exists")
	}
	if !os.IsNotExist(err) {
		return errors.New("inspect evaluation receipt destination")
	}
	return nil
}

func validEvaluationReceiptDirectory(path string) bool {
	return len(path) > 0 && len(path) <= maximumRootDirectoryBytes && utf8.ValidString(path) &&
		!containsControl(path) && filepath.IsAbs(path) && filepath.Clean(path) == path &&
		path != filepath.Dir(path)
}
