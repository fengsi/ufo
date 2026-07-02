package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type zeroReader struct {
	n int64
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func (r *zeroReader) Read(p []byte) (int, error) {
	if r.n == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.n {
		p = p[:r.n]
	}
	for i := range p {
		p[i] = 0
	}
	r.n -= int64(len(p))
	return len(p), nil
}

func TestAssetUploadLifecycle(t *testing.T) {
	t.Setenv("UFO_HUB_ASSET_BACKEND", assetBackendLocal)
	t.Setenv("UFO_HUB_ASSET_LOCAL_ROOT", t.TempDir())

	ts, srv := newTestServerWithNotifier(t)
	owner := signup(t, ts, "asset-owner")
	outsider := signup(t, ts, "asset-outsider")

	code, b := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Assets"})
	if code != http.StatusCreated {
		t.Fatalf("create fleet: %d %s", code, b)
	}
	fleetID := field(t, b, "id")

	code, b = do(t, owner, "POST", ts.URL+"/v1/assets", "", map[string]any{
		"fleet_id":     fleetID,
		"filename":     "../trace.log",
		"content_type": "text/plain",
		"byte_size":    len("hello asset\n"),
	})
	if code != http.StatusCreated {
		t.Fatalf("create upload: %d %s", code, b)
	}
	var intent struct {
		AssetID string            `json:"asset_id"`
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(b, &intent); err != nil {
		t.Fatalf("decode upload intent: %v", err)
	}
	if intent.Method != http.MethodPut || !strings.HasPrefix(intent.URL, "/v1/assets/") || !strings.HasSuffix(intent.URL, "/file") {
		t.Fatalf("upload target = %s %s", intent.Method, intent.URL)
	}

	if code, b := do(t, outsider, "PATCH", ts.URL+"/v1/assets/"+intent.AssetID, "", map[string]string{"status": "ready"}); code != http.StatusForbidden {
		t.Fatalf("outsider complete: %d %s", code, b)
	}

	uploadURL := ts.URL + strings.TrimPrefix(intent.URL, "/api")
	req, err := http.NewRequest(http.MethodPut, uploadURL, bytes.NewReader([]byte("hello asset\n")))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range intent.Headers {
		req.Header.Set(k, v)
	}
	res, err := owner.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("upload bytes: %d", res.StatusCode)
	}

	code, b = do(t, owner, "PATCH", ts.URL+"/v1/assets/"+intent.AssetID, "", map[string]string{"status": "ready"})
	if code != http.StatusOK {
		t.Fatalf("complete upload: %d %s", code, b)
	}
	var asset struct {
		ID          string            `json:"id"`
		Filename    string            `json:"filename"`
		ContentType string            `json:"content_type"`
		ByteSize    int64             `json:"byte_size"`
		Checksums   map[string]string `json:"checksums"`
		URL         string            `json:"url"`
		Metadata    map[string]any    `json:"metadata"`
	}
	if err := json.Unmarshal(b, &asset); err != nil {
		t.Fatalf("decode asset: %v", err)
	}
	if asset.ID != intent.AssetID || asset.Filename != "trace.log" || asset.ByteSize != int64(len("hello asset\n")) {
		t.Fatalf("asset response: %+v", asset)
	}
	if asset.URL != "/v1/assets/"+asset.ID+"/file" {
		t.Fatalf("asset url: %s", asset.URL)
	}
	if !validBlake3(asset.Checksums["blake3"]) {
		t.Fatalf("asset metadata/hash: %+v", asset)
	}

	pid, ok := parseUUID(asset.ID)
	if !ok {
		t.Fatalf("asset id is not uuid: %s", asset.ID)
	}
	row, err := srv.q.GetAssetByPublicID(context.Background(), pid)
	if err != nil {
		t.Fatalf("get asset row: %v", err)
	}
	if !strings.HasPrefix(row.ObjectKey, "v1/fleets/"+fleetID+"/uploads/") || strings.Contains(row.ObjectKey, "/operation/") {
		t.Fatalf("object key has wrong owner path: %s", row.ObjectKey)
	}

	req, err = http.NewRequest(http.MethodGet, ts.URL+strings.TrimPrefix(asset.URL, "/api"), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err = owner.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || string(got) != "hello asset\n" {
		t.Fatalf("content: %d %q", res.StatusCode, got)
	}
}

func TestCreateAssetHonorsSizeAndContentTypeConfig(t *testing.T) {
	t.Setenv("UFO_HUB_ASSET_BACKEND", assetBackendLocal)
	t.Setenv("UFO_HUB_ASSET_LOCAL_ROOT", t.TempDir())
	t.Setenv("UFO_HUB_ASSET_UPLOAD_MAX_BYTES", "16")
	t.Setenv("UFO_HUB_ASSET_UPLOAD_ALLOWED_CONTENT_TYPES", "application/pdf,image/*")

	ts := newTestServer(t)
	owner := signup(t, ts, "asset-limits")
	_, b := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Assets"})
	fleetID := field(t, b, "id")

	code, body := do(t, owner, "POST", ts.URL+"/v1/assets", "", map[string]any{
		"fleet_id": fleetID, "filename": "big.pdf", "content_type": "application/pdf", "byte_size": 17,
	})
	if code != http.StatusBadRequest || !strings.Contains(string(body), "exceeds") {
		t.Fatalf("large upload: %d %s", code, body)
	}

	code, body = do(t, owner, "POST", ts.URL+"/v1/assets", "", map[string]any{
		"fleet_id": fleetID, "filename": "note.txt", "content_type": "text/plain", "byte_size": 1,
	})
	if code != http.StatusBadRequest || !strings.Contains(string(body), "content type") {
		t.Fatalf("disallowed type: %d %s", code, body)
	}

	code, body = do(t, owner, "POST", ts.URL+"/v1/assets", "", map[string]any{
		"fleet_id": fleetID, "filename": "image.png", "content_type": "image/png", "byte_size": 1,
	})
	if code != http.StatusCreated {
		t.Fatalf("allowed wildcard type: %d %s", code, body)
	}
}

func TestAssetFileUploadStreamsPastJSONBodyLimit(t *testing.T) {
	t.Setenv("UFO_HUB_ASSET_BACKEND", assetBackendLocal)
	t.Setenv("UFO_HUB_ASSET_LOCAL_ROOT", t.TempDir())

	ts := newTestServer(t)
	owner := signup(t, ts, "asset-large")
	_, b := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Assets"})
	fleetID := field(t, b, "id")
	size := int64(maxLargeBody + 1)

	code, b := do(t, owner, "POST", ts.URL+"/v1/assets", "", map[string]any{
		"fleet_id": fleetID, "filename": "large.bin", "content_type": "application/octet-stream", "byte_size": size,
	})
	if code != http.StatusCreated {
		t.Fatalf("create large upload: %d %s", code, b)
	}
	var intent struct {
		AssetID string            `json:"asset_id"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(b, &intent); err != nil {
		t.Fatalf("decode large upload intent: %v", err)
	}
	req, err := http.NewRequest(http.MethodPut, ts.URL+strings.TrimPrefix(intent.URL, "/api"), &zeroReader{n: size})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range intent.Headers {
		req.Header.Set(k, v)
	}
	res, err := owner.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("upload large content: %d", res.StatusCode)
	}
	code, b = do(t, owner, "PATCH", ts.URL+"/v1/assets/"+intent.AssetID, "", map[string]string{"status": "ready"})
	if code != http.StatusOK {
		t.Fatalf("complete large upload: %d %s", code, b)
	}
}

func TestObjectStoreUploadTargetUsesPutURL(t *testing.T) {
	ts, srv := newTestServerWithNotifier(t)
	store := &fakeObjectStore{objects: map[string][]byte{}}
	srv.assets = store
	srv.assetStores = map[string]assetStore{store.Backend(): store}

	owner := signup(t, ts, "asset-put-url")
	_, b := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Assets"})
	fleetID := field(t, b, "id")
	code, b := do(t, owner, "POST", ts.URL+"/v1/assets", "", map[string]any{
		"fleet_id": fleetID, "filename": "trace.pdf", "content_type": "application/pdf", "byte_size": 123,
	})
	if code != http.StatusCreated {
		t.Fatalf("create object store upload: %d %s", code, b)
	}
	var intent struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(b, &intent); err != nil {
		t.Fatalf("decode object store upload intent: %v", err)
	}
	if intent.Method != http.MethodPut || !strings.HasPrefix(intent.URL, "https://objects.example/") || intent.Headers["Content-Type"] != "application/pdf" {
		t.Fatalf("object store upload target: %+v", intent)
	}
}

func TestAssetUploadHeadersDoNotExposeContentLength(t *testing.T) {
	headers := assetUploadHeaders(http.Header{
		"Host":           []string{"bucket.example"},
		"Content-Length": []string{"123"},
		"Content-Type":   []string{"application/pdf"},
	})
	if _, ok := headers["Content-Length"]; ok {
		t.Fatalf("content-length should not be client-settable: %+v", headers)
	}
	if headers["Content-Type"] != "application/pdf" {
		t.Fatalf("content type header missing: %+v", headers)
	}
}

func TestS3PresignUploadSignsSizeAndContentType(t *testing.T) {
	client := s3.NewFromConfig(aws.Config{
		Region:      "auto",
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("test-access", "test-secret", "")),
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("https://account.r2.cloudflarestorage.com")
		o.UsePathStyle = true
	})
	store := s3AssetStore{
		client:    client,
		presign:   s3.NewPresignClient(client),
		bucket:    "bucket",
		expiresIn: time.Minute,
	}
	target, err := store.PresignUpload(context.Background(), "v1/fleets/f/aa/id", assetPutOptions{
		ContentType: "application/pdf",
		ByteSize:    123,
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	signedHeaders := u.Query().Get("X-Amz-SignedHeaders")
	if target.Method != http.MethodPut || target.Headers["Content-Type"] != "application/pdf" || !strings.Contains(signedHeaders, "content-type") || !strings.Contains(signedHeaders, "content-length") {
		t.Fatalf("presigned upload target=%+v signed_headers=%q", target, signedHeaders)
	}
}

func TestGCSPresignUploadSignsSizeAndContentType(t *testing.T) {
	key := testRSAKey(t)
	store := &gcsAssetStore{
		bucket:     "bucket",
		endpoint:   gcsDefaultEndpoint,
		expiresIn:  time.Minute,
		account:    gcsServiceAccount{ClientEmail: "svc@example.iam.gserviceaccount.com"},
		privateKey: key,
	}
	target, err := store.PresignUpload(context.Background(), "v1/fleets/f/aa/id", assetPutOptions{
		ContentType: "application/pdf",
		ByteSize:    123,
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	signedHeaders := u.Query().Get("X-Goog-SignedHeaders")
	if target.Method != http.MethodPut || target.Headers["Content-Type"] != "application/pdf" || !strings.Contains(signedHeaders, "content-type") || !strings.Contains(signedHeaders, "content-length") || !strings.Contains(signedHeaders, "host") {
		t.Fatalf("gcs upload target=%+v signed_headers=%q", target, signedHeaders)
	}
	if u.Query().Get("X-Goog-Algorithm") != "GOOG4-RSA-SHA256" || !strings.Contains(u.Query().Get("X-Goog-Credential"), "svc@example.iam.gserviceaccount.com/") {
		t.Fatalf("gcs signed url query=%v", u.Query())
	}
}

func TestGCSAssetStoreRoundTrip(t *testing.T) {
	key := testRSAKey(t)
	objects := map[string][]byte{}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "oauth2.test" && r.URL.Path == "/token" {
			return testHTTPResponse(r, http.StatusOK, http.Header{"Content-Type": []string{"application/json"}}, `{"access_token":"test-token","expires_in":3600}`), nil
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			return nil, fmt.Errorf("missing bearer token: %q", r.Header.Get("Authorization"))
		}
		objectKey := strings.TrimPrefix(r.URL.Path, "/bucket/")
		switch r.Method {
		case http.MethodPut:
			if r.Header.Get("Content-Type") != "text/plain" || r.ContentLength != 5 {
				return nil, fmt.Errorf("bad put headers: content-type=%q length=%d", r.Header.Get("Content-Type"), r.ContentLength)
			}
			b, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			objects[objectKey] = b
			return testHTTPResponse(r, http.StatusOK, nil, ""), nil
		case http.MethodHead:
			body, ok := objects[objectKey]
			if !ok {
				return testHTTPResponse(r, http.StatusNotFound, nil, ""), nil
			}
			return testHTTPResponse(r, http.StatusOK, http.Header{
				"Content-Length": []string{fmt.Sprintf("%d", len(body))},
				"X-Goog-Hash":    []string{"crc32c=AAAAAA==,md5=1B2M2Y8AsgTpgAmY7PhCfg=="},
			}, ""), nil
		case http.MethodGet:
			body, ok := objects[objectKey]
			if !ok {
				return testHTTPResponse(r, http.StatusNotFound, nil, ""), nil
			}
			return testHTTPResponse(r, http.StatusOK, nil, string(body)), nil
		case http.MethodDelete:
			delete(objects, objectKey)
			return testHTTPResponse(r, http.StatusNoContent, nil, ""), nil
		default:
			return nil, fmt.Errorf("unexpected method %s", r.Method)
		}
	})}

	store := &gcsAssetStore{
		httpClient: client,
		bucket:     "bucket",
		endpoint:   "https://storage.test",
		expiresIn:  time.Minute,
		account:    gcsServiceAccount{ClientEmail: "svc@example.iam.gserviceaccount.com", TokenURI: "https://oauth2.test/token"},
		privateKey: key,
	}
	n, err := store.PutReader(context.Background(), "v1/fleets/f/aa/id", strings.NewReader("hello"), assetPutOptions{ContentType: "text/plain", ByteSize: 5})
	if err != nil || n != 5 {
		t.Fatalf("put: n=%d err=%v", n, err)
	}
	stat, err := store.Stat(context.Background(), "v1/fleets/f/aa/id")
	if err != nil || stat.ByteSize != 5 || stat.Checksums["crc32c"] != "00000000" || stat.Checksums["md5"] != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Fatalf("stat=%+v err=%v", stat, err)
	}
	rc, err := store.Open(context.Background(), "v1/fleets/f/aa/id")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "hello" {
		t.Fatalf("open body=%q err=%v", body, err)
	}
	if err := store.Delete(context.Background(), "v1/fleets/f/aa/id"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stat(context.Background(), "v1/fleets/f/aa/id"); err == nil {
		t.Fatal("stat after delete succeeded")
	}
}

func testHTTPResponse(req *http.Request, status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	contentLength := int64(len(body))
	if v := header.Get("Content-Length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			contentLength = n
		}
	}
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		Request:       req,
		ContentLength: contentLength,
	}
}

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testServiceAccountJSON(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	b, err := json.Marshal(gcsServiceAccount{
		ClientEmail: "svc@example.iam.gserviceaccount.com",
		PrivateKey:  string(pemBytes),
		TokenURI:    gcsDefaultTokenURI,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAcceptedRunIncludesAssetURLs(t *testing.T) {
	t.Setenv("UFO_HUB_ASSET_BACKEND", assetBackendLocal)
	t.Setenv("UFO_HUB_ASSET_LOCAL_ROOT", t.TempDir())

	ts := newTestServer(t)
	owner := signup(t, ts, "asset-accept")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Assets"})
	fleetID := field(t, fb, "id")
	assetID := uploadTextAsset(t, owner, ts.URL, fleetID, "trace.log", "hello pilot\n")

	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fleetID, "name": "r"})
	enrollmentCode := field(t, tb, "code")
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", enrollmentCode, map[string]any{"name": "r", "auto_tags": []string{"pilot:claude"}})
	connectionToken := field(t, eb, "token")
	roverID := field(t, eb, "id")
	if code, b := do(t, &http.Client{}, "PATCH", ts.URL+"/v1/rovers/"+roverID, connectionToken, map[string]any{"auto_tags": []string{"pilot:claude"}}); code != http.StatusNoContent {
		t.Fatalf("touch rover: %d %s", code, b)
	}

	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fleetID, "name": "M", "key": "M"})
	missionID := field(t, mb, "id")
	body := "see [trace](/v1/assets/" + assetID + "/file)"
	code, b := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{
		"fleet_id": fleetID, "title": "asset op", "body": body, "mission_id": missionID,
		"assignee_type": "pilot", "assignee_id": "claude",
	})
	if code != http.StatusCreated {
		t.Fatalf("create operation: %d %s", code, b)
	}
	operationID := field(t, b, "id")
	contextAssetID := uploadTextAsset(t, owner, ts.URL, fleetID, "context.txt", "operation context\n", operationID)

	code, b = do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", connectionToken, nil)
	if code != http.StatusOK {
		t.Fatalf("accept: %d %s", code, b)
	}
	var accept struct {
		ID     string `json:"id"`
		Prompt string `json:"prompt"`
		Assets []struct {
			ID       string `json:"id"`
			Filename string `json:"filename"`
			URL      string `json:"url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(b, &accept); err != nil {
		t.Fatalf("decode accept: %v (%s)", err, b)
	}
	if !strings.Contains(accept.Prompt, assetID) || len(accept.Assets) != 2 || accept.Assets[0].ID != assetID || accept.Assets[0].Filename != "trace.log" || accept.Assets[0].URL != "/v1/assets/"+assetID+"/file" || accept.Assets[1].ID != contextAssetID || accept.Assets[1].Filename != "context.txt" {
		t.Fatalf("accept assets: %+v prompt=%q", accept.Assets, accept.Prompt)
	}
	code, b = do(t, &http.Client{}, "GET", ts.URL+"/v1/assets/"+assetID+"/file", connectionToken, nil)
	if code != http.StatusOK || string(b) != "hello pilot\n" {
		t.Fatalf("asset content: %d %q", code, b)
	}
	code, b = do(t, &http.Client{}, "GET", ts.URL+"/v1/assets/"+assetID, connectionToken, nil)
	if code != http.StatusOK || !strings.Contains(string(b), `"/v1/assets/`+assetID+`/file"`) {
		t.Fatalf("asset metadata: %d %s", code, b)
	}

	unrelatedID := uploadTextAsset(t, owner, ts.URL, fleetID, "secret.txt", "fleet secret\n")
	if code, b := do(t, &http.Client{}, "GET", ts.URL+"/v1/assets/"+unrelatedID+"/file", connectionToken, nil); code != http.StatusForbidden {
		t.Fatalf("rover read of unrelated asset: %d %s", code, b)
	}

	if code, b := do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/"+accept.ID+"/result", connectionToken, map[string]any{
		"status": "succeeded", "message": "done", "session_id": "asset-session",
	}); code != http.StatusNoContent {
		t.Fatalf("run result: %d %s", code, b)
	}
	if code, b := postOperationComment(t, owner, ts.URL, operationID, "please inspect the same file"); code != http.StatusCreated {
		t.Fatalf("comment: %d %s", code, b)
	}
	code, b = do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", connectionToken, nil)
	if code != http.StatusOK {
		t.Fatalf("resume accept: %d %s", code, b)
	}
	var resume struct {
		Prompt string `json:"prompt"`
		Assets []struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(b, &resume); err != nil {
		t.Fatalf("decode resume accept: %v (%s)", err, b)
	}
	if strings.Contains(resume.Prompt, assetID) || len(resume.Assets) != 2 || resume.Assets[0].ID != assetID || resume.Assets[0].URL != "/v1/assets/"+assetID+"/file" || resume.Assets[1].ID != contextAssetID {
		t.Fatalf("resume accept assets: %+v prompt=%q", resume.Assets, resume.Prompt)
	}
}

func TestResolveAndListOperationAssets(t *testing.T) {
	assetRoot := t.TempDir()
	t.Setenv("UFO_HUB_ASSET_BACKEND", assetBackendLocal)
	t.Setenv("UFO_HUB_ASSET_LOCAL_ROOT", assetRoot)

	ts, srv := newTestServerWithNotifier(t)
	owner := signup(t, ts, "asset-list-owner")
	outsider := signup(t, ts, "asset-list-outsider")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Assets"})
	fleetID := field(t, fb, "id")
	assetA := uploadTextAsset(t, owner, ts.URL, fleetID, "a.txt", "a\n")
	assetB := uploadTextAsset(t, owner, ts.URL, fleetID, "b.txt", "b\n")
	assetLinkedOnCreate := uploadTextAsset(t, owner, ts.URL, fleetID, "draft-only.txt", "draft only\n")

	_, ofb := do(t, outsider, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Other Assets"})
	otherFleetID := field(t, ofb, "id")
	otherAsset := uploadTextAsset(t, outsider, ts.URL, otherFleetID, "other.txt", "other\n")

	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fleetID, "name": "M", "key": "M"})
	missionID := field(t, mb, "id")
	body := "body [a](/v1/assets/" + assetA + "/file)"
	code, b := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{
		"fleet_id": fleetID, "title": "asset list op", "body": body, "mission_id": missionID,
		"assignee_type": "pilot", "assignee_id": "claude", "asset_ids": []string{assetLinkedOnCreate},
	})
	if code != http.StatusCreated {
		t.Fatalf("create operation: %d %s", code, b)
	}
	operationID := field(t, b, "id")
	code, b = do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{
		"fleet_id": fleetID, "title": "asset list sub-op", "mission_id": missionID, "main_operation_id": operationID,
	})
	if code != http.StatusCreated {
		t.Fatalf("create sub-operation: %d %s", code, b)
	}
	subOperationID := field(t, b, "id")
	comment := "comment [b](/v1/assets/" + assetB + "/file) [a again](/v1/assets/" + assetA + "/file)"
	if code, b := postOperationComment(t, owner, ts.URL, operationID, comment); code != http.StatusCreated {
		t.Fatalf("comment: %d %s", code, b)
	}
	assetC := uploadTextAsset(t, owner, ts.URL, fleetID, "c.txt", "c\n", operationID)
	assetD := uploadTextAsset(t, owner, ts.URL, fleetID, "d.txt", "d\n", subOperationID)

	code, b = do(t, owner, "GET", ts.URL+"/v1/assets?operation_id="+operationID, "", nil)
	if code != http.StatusOK {
		t.Fatalf("operation assets: %d %s", code, b)
	}
	var opAssets []struct {
		ID       string `json:"id"`
		Filename string `json:"filename"`
		URL      string `json:"url"`
	}
	if err := json.Unmarshal(b, &opAssets); err != nil {
		t.Fatalf("decode operation assets: %v (%s)", err, b)
	}
	wantAssets := []string{assetA, assetB, assetLinkedOnCreate, assetC, assetD}
	if len(opAssets) != len(wantAssets) || opAssets[0].URL != "/v1/assets/"+assetA+"/file" {
		t.Fatalf("operation assets = %+v", opAssets)
	}
	for i, want := range wantAssets {
		if opAssets[i].ID != want {
			t.Fatalf("operation assets = %+v", opAssets)
		}
	}

	code, b = do(t, owner, "POST", ts.URL+"/v1/assets/resolve", "", map[string]any{"ids": []string{assetB, assetA, assetB, otherAsset}})
	if code != http.StatusOK {
		t.Fatalf("resolve assets: %d %s", code, b)
	}
	var resolved []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &resolved); err != nil {
		t.Fatalf("decode resolved assets: %v (%s)", err, b)
	}
	if len(resolved) != 2 || resolved[0].ID != assetB || resolved[1].ID != assetA {
		t.Fatalf("resolved assets = %+v", resolved)
	}

	assetAPublicID, ok := parseUUID(assetA)
	if !ok {
		t.Fatalf("asset id is not uuid: %s", assetA)
	}
	assetARow, err := srv.q.GetAssetByPublicID(context.Background(), assetAPublicID)
	if err != nil {
		t.Fatalf("get asset row: %v", err)
	}
	assetAPath := filepath.Join(assetRoot, assetARow.ObjectKey)
	if _, err := os.Stat(assetAPath); err != nil {
		t.Fatalf("asset file before delete: %v", err)
	}
	if code, b := do(t, owner, "DELETE", ts.URL+"/v1/assets/"+assetA, "", nil); code != http.StatusNoContent {
		t.Fatalf("delete asset: %d %s", code, b)
	}
	if _, err := os.Stat(assetAPath); !os.IsNotExist(err) {
		t.Fatalf("asset file after delete: %v", err)
	}
	if code, b := do(t, owner, "GET", ts.URL+"/v1/assets/"+assetA+"/file", "", nil); code != http.StatusNotFound {
		t.Fatalf("deleted asset file: %d %s", code, b)
	}
}

func TestRoverCanCreateAssetForAcceptedRun(t *testing.T) {
	t.Setenv("UFO_HUB_ASSET_BACKEND", assetBackendLocal)
	t.Setenv("UFO_HUB_ASSET_LOCAL_ROOT", t.TempDir())

	ts, srv := newTestServerWithNotifier(t)
	owner := signup(t, ts, "asset-rover")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Assets"})
	fleetID := field(t, fb, "id")
	_, tb := do(t, owner, "POST", ts.URL+"/v1/enrollment-codes", "", map[string]any{"fleet_id": fleetID, "name": "r"})
	_, eb := do(t, &http.Client{}, "POST", ts.URL+"/v1/rovers", field(t, tb, "code"), map[string]any{"name": "r", "auto_tags": []string{"pilot:claude"}})
	connectionToken := field(t, eb, "token")

	_, mb := do(t, owner, "POST", ts.URL+"/v1/missions", "", map[string]string{"fleet_id": fleetID, "name": "M", "key": "M"})
	code, b := do(t, owner, "POST", ts.URL+"/v1/operations", "", map[string]any{
		"fleet_id": fleetID, "title": "asset rover op", "mission_id": field(t, mb, "id"),
		"assignee_type": "pilot", "assignee_id": "claude",
	})
	if code != http.StatusCreated {
		t.Fatalf("create operation: %d %s", code, b)
	}
	operationID := field(t, b, "id")
	code, b = do(t, &http.Client{}, "POST", ts.URL+"/v1/runs/accept", connectionToken, nil)
	if code != http.StatusOK {
		t.Fatalf("accept: %d %s", code, b)
	}
	runID := field(t, b, "id")

	assetID := uploadRoverTextAsset(t, connectionToken, ts.URL, runID, "report.txt", "generated\n")
	code, b = do(t, owner, "GET", ts.URL+"/v1/assets?operation_id="+operationID, "", nil)
	if code != http.StatusOK {
		t.Fatalf("operation assets: %d %s", code, b)
	}
	if !strings.Contains(string(b), assetID) || !strings.Contains(string(b), "report.txt") {
		t.Fatalf("operation assets missing rover asset %s: %s", assetID, b)
	}
	pid, ok := parseUUID(assetID)
	if !ok {
		t.Fatalf("asset id is not uuid: %s", assetID)
	}
	row, err := srv.q.GetAssetByPublicID(context.Background(), pid)
	if err != nil {
		t.Fatalf("get rover asset row: %v", err)
	}
	if !strings.HasPrefix(row.ObjectKey, "v1/fleets/"+fleetID+"/runs/") ||
		!strings.Contains(row.ObjectKey, "/artifacts/"+assetID[:2]+"/"+assetID) {
		t.Fatalf("rover asset object key: %s", row.ObjectKey)
	}
}

func TestAssetFileRedirectsForObjectStore(t *testing.T) {
	ts, srv := newTestServerWithNotifier(t)
	store := &fakeObjectStore{objects: map[string][]byte{}}
	srv.assets = store
	srv.assetStores = map[string]assetStore{store.Backend(): store}

	owner := signup(t, ts, "asset-s3")
	_, fb := do(t, owner, "POST", ts.URL+"/v1/fleets", "", map[string]string{"name": "Assets"})
	fleetID := field(t, fb, "id")
	fpid, ok := parseUUID(fleetID)
	if !ok {
		t.Fatalf("fleet id is not uuid: %s", fleetID)
	}
	fleet, err := srv.q.GetFleetByPublicID(context.Background(), fpid)
	if err != nil {
		t.Fatalf("get fleet: %v", err)
	}
	ownerPath, err := assetOwnerForFleet(context.Background(), srv.q, fleet.ID)
	if err != nil {
		t.Fatalf("asset owner: %v", err)
	}
	asset, err := srv.storeAssetBytes(context.Background(), srv.q, ownerPath, "trace.log", "text/plain", []byte("hello object store\n"), 0, nil)
	if err != nil {
		t.Fatalf("store asset: %v", err)
	}
	assetID := uuidStr(asset.PublicID)

	client := &http.Client{Jar: owner.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/assets/"+assetID+"/file", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusFound || !strings.HasPrefix(res.Header.Get("Location"), "https://objects.example/") {
		t.Fatalf("object store content: status=%d location=%q", res.StatusCode, res.Header.Get("Location"))
	}
	if code, b := do(t, owner, "DELETE", ts.URL+"/v1/assets/"+assetID, "", nil); code != http.StatusNoContent {
		t.Fatalf("delete object store asset: %d %s", code, b)
	}
	if store.deletes != 0 {
		t.Fatalf("object store delete calls = %d", store.deletes)
	}
	if _, ok := store.objects[asset.ObjectKey]; !ok {
		t.Fatalf("object store asset bytes were deleted")
	}
}

func TestUserAssetContextUsesUserPath(t *testing.T) {
	t.Setenv("UFO_HUB_ASSET_BACKEND", assetBackendLocal)
	t.Setenv("UFO_HUB_ASSET_LOCAL_ROOT", t.TempDir())

	ts, srv := newTestServerWithNotifier(t)
	owner := signup(t, ts, "asset-user-context")
	_, me := do(t, owner, "GET", ts.URL+"/v1/users/me", "", nil)
	userID := field(t, me, "id")

	code, b := do(t, owner, "POST", ts.URL+"/v1/assets", "", map[string]any{
		"context":   map[string]string{"user_id": userID},
		"filename":  "me.png",
		"byte_size": len("png"),
	})
	if code != http.StatusCreated {
		t.Fatalf("create user upload: %d %s", code, b)
	}
	var intent struct {
		AssetID string            `json:"asset_id"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(b, &intent); err != nil {
		t.Fatalf("decode user upload intent: %v", err)
	}
	req, err := http.NewRequest(http.MethodPut, ts.URL+strings.TrimPrefix(intent.URL, "/api"), bytes.NewReader([]byte("png")))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range intent.Headers {
		req.Header.Set(k, v)
	}
	res, err := owner.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("upload user bytes: %d", res.StatusCode)
	}
	code, b = do(t, owner, "PATCH", ts.URL+"/v1/assets/"+intent.AssetID, "", map[string]string{"status": "ready"})
	if code != http.StatusOK {
		t.Fatalf("complete user upload: %d %s", code, b)
	}
	var asset struct {
		CreatedBy *string `json:"created_by"`
	}
	if err := json.Unmarshal(b, &asset); err != nil {
		t.Fatalf("decode completed asset: %v", err)
	}
	if asset.CreatedBy == nil || *asset.CreatedBy != userID {
		t.Fatalf("asset created_by = %v, want %s", asset.CreatedBy, userID)
	}

	pid, ok := parseUUID(intent.AssetID)
	if !ok {
		t.Fatalf("asset id is not uuid: %s", intent.AssetID)
	}
	row, err := srv.q.GetAssetByPublicID(context.Background(), pid)
	if err != nil {
		t.Fatalf("get user asset row: %v", err)
	}
	if !strings.HasPrefix(row.ObjectKey, "v1/users/"+userID+"/uploads/") {
		t.Fatalf("user asset object key: %s", row.ObjectKey)
	}
	if row.FleetID.Valid || !row.CreatedBy.Valid {
		t.Fatalf("user asset owner: %+v", row)
	}
	if got := fmt.Sprint(assetMetadataMap(row.Metadata)["user_id"]); got != userID {
		t.Fatalf("user metadata id = %q, want %q", got, userID)
	}
}

func uploadTextAsset(t *testing.T, client *http.Client, baseURL, fleetID, filename, content string, operationIDs ...string) string {
	t.Helper()
	body := map[string]any{
		"fleet_id": fleetID, "filename": filename, "content_type": "text/plain", "byte_size": len(content),
	}
	if len(operationIDs) > 0 && operationIDs[0] != "" {
		body["context"] = map[string]string{"operation_id": operationIDs[0]}
	}
	code, b := do(t, client, "POST", baseURL+"/v1/assets", "", body)
	if code != http.StatusCreated {
		t.Fatalf("create upload: %d %s", code, b)
	}
	var intent struct {
		AssetID string            `json:"asset_id"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(b, &intent); err != nil {
		t.Fatalf("decode upload intent: %v", err)
	}
	req, err := http.NewRequest(http.MethodPut, baseURL+strings.TrimPrefix(intent.URL, "/api"), strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range intent.Headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("upload bytes: %d", res.StatusCode)
	}
	code, b = do(t, client, "PATCH", baseURL+"/v1/assets/"+intent.AssetID, "", map[string]string{"status": "ready"})
	if code != http.StatusOK {
		t.Fatalf("complete upload: %d %s", code, b)
	}
	return intent.AssetID
}

func uploadRoverTextAsset(t *testing.T, token, baseURL, runID, filename, content string) string {
	t.Helper()
	code, b := do(t, &http.Client{}, "POST", baseURL+"/v1/assets", token, map[string]any{
		"context": map[string]string{"run_id": runID}, "filename": filename, "content_type": "text/plain", "byte_size": len(content),
	})
	if code != http.StatusCreated {
		t.Fatalf("create rover upload: %d %s", code, b)
	}
	var intent struct {
		AssetID string            `json:"asset_id"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(b, &intent); err != nil {
		t.Fatalf("decode rover upload intent: %v", err)
	}
	req, err := http.NewRequest(http.MethodPut, baseURL+strings.TrimPrefix(intent.URL, "/api"), strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(roverVersionHeader, currentRoverVersion)
	for k, v := range intent.Headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("upload rover bytes: %d", res.StatusCode)
	}
	code, b = do(t, &http.Client{}, "PATCH", baseURL+"/v1/assets/"+intent.AssetID, token, map[string]string{"status": "ready"})
	if code != http.StatusOK {
		t.Fatalf("complete rover upload: %d %s", code, b)
	}
	return intent.AssetID
}

type fakeObjectStore struct {
	objects map[string][]byte
	deletes int
}

func (s *fakeObjectStore) Backend() string { return assetBackendS3 }

func (s *fakeObjectStore) Put(_ context.Context, key string, body []byte, _ assetPutOptions) error {
	s.objects[key] = append([]byte(nil), body...)
	return nil
}

func (s *fakeObjectStore) PutReader(_ context.Context, key string, body io.Reader, _ assetPutOptions) (int64, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return int64(len(b)), err
	}
	s.objects[key] = append([]byte(nil), b...)
	return int64(len(b)), nil
}

func (s *fakeObjectStore) PresignUpload(_ context.Context, key string, opts assetPutOptions) (assetUploadTarget, error) {
	return assetUploadTarget{
		Method: http.MethodPut, URL: "https://objects.example/" + key,
		Headers:   map[string]string{"Content-Type": opts.ContentType},
		ExpiresAt: time.Now().Add(time.Minute),
	}, nil
}

func (s *fakeObjectStore) PresignGet(_ context.Context, key string, _ assetGetOptions) (assetUploadTarget, error) {
	if _, ok := s.objects[key]; !ok {
		return assetUploadTarget{}, fmt.Errorf("missing object")
	}
	return assetUploadTarget{Method: http.MethodGet, URL: "https://objects.example/" + key, ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (s *fakeObjectStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	body, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("missing object")
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func (s *fakeObjectStore) Delete(_ context.Context, key string) error {
	s.deletes++
	delete(s.objects, key)
	return nil
}

func (s *fakeObjectStore) Stat(_ context.Context, key string) (assetStat, error) {
	body, ok := s.objects[key]
	if !ok {
		return assetStat{}, fmt.Errorf("missing object")
	}
	return assetStat{ByteSize: int64(len(body))}, nil
}
