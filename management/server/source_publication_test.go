package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/management"
)

const serverSourceRootIdentity = "sha256:3333333333333333333333333333333333333333333333333333333333333333"

func TestSourcePublicationAPISeparatesCreateAndUpdateAuthority(t *testing.T) {
	root := t.TempDir()
	publication, err := management.NewRootedSourcePublisher(management.RootedSourcePublisherOptions{
		Root: root, RootIdentity: serverSourceRootIdentity,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := publication.Close(); err != nil {
			t.Errorf("close source publisher: %v", err)
		}
	})
	authority := management.NewCapabilityRegistry()
	createAccess, err := authority.Issue(time.Minute, []management.Grant{{
		Operation: management.CreateSource, Resource: serverSourceRootIdentity,
	}})
	if err != nil {
		t.Fatal(err)
	}
	updateAccess, err := authority.Issue(time.Minute, []management.Grant{{
		Operation: management.UpdateSource, Resource: serverSourceRootIdentity,
	}})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := NewBundle(BundleConfig{
		Authorizer: authority, SourcePublication: publication,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := bundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := mounted.Close(ctx); err != nil {
			t.Errorf("close source-publication management bundle: %v", err)
		}
	})
	handler, err := HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	path := management.APIPrefix + "/authoring/write"
	create := management.SourceWriteRequest{
		FormatVersion: management.SourceWriteFormatVersion,
		RootIdentity:  serverSourceRootIdentity,
		Mode:          management.SourceCreate,
		Path:          "server.ortg",
		Source:        "graph server_create {\n}\n",
	}
	createBody, err := json.Marshal(create)
	if err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{
		"absent": "", "wrong-operation": updateAccess.Token,
	} {
		response := request(t, handler, http.MethodPost, path, token, createBody)
		if response.Code != http.StatusNotFound {
			t.Errorf("%s create authority status = %d body=%s", name, response.Code, response.Body.String())
		}
	}
	if _, err := os.Stat(filepath.Join(root, create.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unauthorized request published a source: %v", err)
	}
	response := request(t, handler, http.MethodPost, path, createAccess.Token, createBody)
	if response.Code != http.StatusOK {
		t.Fatalf("authorized create = %d %s", response.Code, response.Body.String())
	}
	var created management.SourceWriteReceipt
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil ||
		management.ValidateSourceWriteReceipt(create, created) != nil {
		t.Fatalf("create receipt = %+v, %v", created, err)
	}
	if repeated := request(t, handler, http.MethodPost, path, createAccess.Token, createBody); repeated.Code != http.StatusConflict {
		t.Fatalf("repeated create = %d %s", repeated.Code, repeated.Body.String())
	}

	update := create
	update.Mode = management.SourceUpdate
	update.Source = "graph server_update {\n}\n"
	update.ExpectedSourceDigest = created.SourceDigest
	updateBody, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	if denied := request(t, handler, http.MethodPost, path, createAccess.Token, updateBody); denied.Code != http.StatusNotFound {
		t.Fatalf("create capability authorized update = %d %s", denied.Code, denied.Body.String())
	}
	response = request(t, handler, http.MethodPost, path, updateAccess.Token, updateBody)
	if response.Code != http.StatusOK {
		t.Fatalf("authorized update = %d %s", response.Code, response.Body.String())
	}
	var updated management.SourceWriteReceipt
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil ||
		management.ValidateSourceWriteReceipt(update, updated) != nil {
		t.Fatalf("update receipt = %+v, %v", updated, err)
	}
	published, err := os.ReadFile(filepath.Join(root, create.Path))
	if err != nil || string(published) != update.Source {
		t.Fatalf("published source = %q, %v", published, err)
	}

	if queried := request(t, handler, http.MethodPost, path+"?unexpected=1", updateAccess.Token, updateBody); queried.Code != http.StatusBadRequest {
		t.Fatalf("query-bearing publication = %d", queried.Code)
	}
	duplicate := []byte(strings.Replace(string(updateBody), `"mode":"update"`,
		`"mode":"update","mode":"update"`, 1))
	if malformed := request(t, handler, http.MethodPost, path, updateAccess.Token, duplicate); malformed.Code != http.StatusBadRequest {
		t.Fatalf("duplicate-key publication = %d %s", malformed.Code, malformed.Body.String())
	}
	if wrongMethod := request(t, handler, http.MethodGet, path, updateAccess.Token, nil); wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong publication method = %d", wrongMethod.Code)
	}
}

type forgedSourcePublication struct{}

func (forgedSourcePublication) Publish(
	_ context.Context, request management.SourceWriteRequest,
) (management.SourceWriteReceipt, error) {
	receipt, err := management.NewSourceWriteReceipt(request, false)
	if err != nil {
		return management.SourceWriteReceipt{}, err
	}
	receipt.SourceDigest = "sha256:" + strings.Repeat("4", 64)
	return receipt, nil
}

func TestSourcePublicationAPIRejectsForgedProviderReceipt(t *testing.T) {
	authority := management.NewCapabilityRegistry()
	access, err := authority.Issue(time.Minute, []management.Grant{{
		Operation: management.CreateSource, Resource: serverSourceRootIdentity,
	}})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := NewBundle(BundleConfig{
		Authorizer: authority, SourcePublication: forgedSourcePublication{},
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := bundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	handler, err := HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	input := management.SourceWriteRequest{
		FormatVersion: management.SourceWriteFormatVersion,
		RootIdentity:  serverSourceRootIdentity,
		Mode:          management.SourceCreate,
		Path:          "forged.ortg",
		Source:        "graph forged {\n}\n",
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/write", access.Token, body)
	if response.Code != http.StatusConflict {
		t.Fatalf("forged provider receipt = %d %s", response.Code, response.Body.String())
	}
}

func TestOperatorAPICanMountOnlyTheMediatedWriteBoundary(t *testing.T) {
	root := t.TempDir()
	publication, err := management.NewRootedSourcePublisher(management.RootedSourcePublisherOptions{
		Root: root, RootIdentity: serverSourceRootIdentity,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = publication.Close() })
	authority := management.NewCapabilityRegistry()
	access, err := authority.Issue(time.Minute, []management.Grant{{
		Operation: management.CreateSource, Resource: serverSourceRootIdentity,
	}})
	if err != nil {
		t.Fatal(err)
	}
	base := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Fallthrough", request.URL.Path)
		writer.WriteHeader(http.StatusNoContent)
	})
	api, err := MountOperatorAPI(context.Background(), base, OperatorAPIConfig{
		Authorizer: authority, SourcePublication: publication,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Close(context.Background()) })
	input := management.SourceWriteRequest{
		FormatVersion: management.SourceWriteFormatVersion,
		RootIdentity:  serverSourceRootIdentity,
		Mode:          management.SourceCreate,
		Path:          "operator.ortg",
		Source:        "graph operator {\n}\n",
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, api.Handler(), http.MethodPost,
		management.APIPrefix+"/authoring/write", access.Token, body)
	if response.Code != http.StatusOK || response.Header().Get("X-Fallthrough") != "" {
		t.Fatalf("operator write overlay = %d header=%q body=%s",
			response.Code, response.Header().Get("X-Fallthrough"), response.Body.String())
	}
	fallthroughRequest := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	fallthroughResponse := httptest.NewRecorder()
	api.Handler().ServeHTTP(fallthroughResponse, fallthroughRequest)
	if fallthroughResponse.Code != http.StatusNoContent ||
		fallthroughResponse.Header().Get("X-Fallthrough") != "/healthz" {
		t.Fatalf("operator write overlay captured base route: %d header=%q",
			fallthroughResponse.Code, fallthroughResponse.Header().Get("X-Fallthrough"))
	}
}

var _ management.SourcePublication = forgedSourcePublication{}
