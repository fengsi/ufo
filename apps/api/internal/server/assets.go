package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/zeebo/blake3"

	"ufo/apps/api/internal/db"
)

const (
	assetBackendLocal          = "local"
	assetBackendS3             = "s3"
	assetBackendGCS            = "gcs"
	assetResolveMaxIDs         = 1000
	defaultAssetUploadMaxBytes = 25 * 1024 * 1024
)

func assetUploadMaxBytes() int64 {
	return int64(envInt("UFO_HUB_ASSET_UPLOAD_MAX_BYTES", defaultAssetUploadMaxBytes))
}

func assetUploadAllowedContentTypes() []string {
	return splitCSV(os.Getenv("UFO_HUB_ASSET_UPLOAD_ALLOWED_CONTENT_TYPES"))
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if v := strings.ToLower(strings.TrimSpace(part)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func boolCount(values ...bool) int {
	n := 0
	for _, v := range values {
		if v {
			n++
		}
	}
	return n
}

type assetOwner struct {
	Path          string
	FleetID       int64
	HasDateInPath bool
}

type assetPutOptions struct {
	ContentType string
	ByteSize    int64
}

type assetGetOptions struct {
	Filename    string
	ContentType string
	Disposition string
}

type assetUploadTarget struct {
	Method    string
	URL       string
	Headers   map[string]string
	ExpiresAt time.Time
}

type assetStat struct {
	ByteSize  int64
	Checksums map[string]string
}

type assetStore interface {
	Backend() string
	Put(context.Context, string, []byte, assetPutOptions) error
	PutReader(context.Context, string, io.Reader, assetPutOptions) (int64, error)
	PresignUpload(context.Context, string, assetPutOptions) (assetUploadTarget, error)
	PresignGet(context.Context, string, assetGetOptions) (assetUploadTarget, error)
	Open(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
	Stat(context.Context, string) (assetStat, error)
}

type localAssetStore struct {
	root string
}

func localAssetRootFromEnv() string {
	if dir := strings.TrimSpace(os.Getenv("UFO_HUB_ASSET_LOCAL_ROOT")); dir != "" {
		return dir
	}
	if dir, err := os.UserHomeDir(); err == nil && strings.TrimSpace(dir) != "" {
		return filepath.Join(dir, ".ufo", "assets")
	}
	return filepath.Join(os.TempDir(), "ufo-assets")
}

func assetStoresFromEnv() (assetStore, map[string]assetStore) {
	local := newLocalAssetStore(localAssetRootFromEnv())
	stores := map[string]assetStore{local.Backend(): local}
	backend := strings.TrimSpace(os.Getenv("UFO_HUB_ASSET_BACKEND"))
	switch backend {
	case "", assetBackendLocal:
		return local, stores
	case assetBackendS3:
		st, err := newS3AssetStore()
		if err != nil {
			panic(err)
		}
		stores[st.Backend()] = st
		return st, stores
	case assetBackendGCS:
		st, err := newGCSAssetStore()
		if err != nil {
			panic(err)
		}
		stores[st.Backend()] = st
		return st, stores
	default:
		panic("unsupported UFO_HUB_ASSET_BACKEND")
	}
}

func (s *Server) assetStore(backend string) (assetStore, error) {
	st, ok := s.assetStores[backend]
	if !ok {
		return nil, fmt.Errorf("unsupported asset backend %q", backend)
	}
	return st, nil
}

func newLocalAssetStore(root string) localAssetStore {
	return localAssetStore{root: root}
}

func (s localAssetStore) Backend() string {
	return assetBackendLocal
}

func (s localAssetStore) path(objectKey string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(objectKey))
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || clean == ".." {
		return "", fmt.Errorf("invalid asset object key")
	}
	return filepath.Join(s.root, clean), nil
}

func (s localAssetStore) Put(_ context.Context, objectKey string, body []byte, _ assetPutOptions) error {
	_, err := s.PutReader(context.Background(), objectKey, bytes.NewReader(body), assetPutOptions{ByteSize: int64(len(body))})
	return err
}

func (s localAssetStore) PutReader(_ context.Context, objectKey string, body io.Reader, _ assetPutOptions) (int64, error) {
	path, err := s.path(objectKey)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	var n int64
	if n, err = io.Copy(tmp, body); err == nil {
		err = tmp.Close()
	} else {
		_ = tmp.Close()
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return n, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return n, err
	}
	return n, nil
}

func (s localAssetStore) PresignUpload(_ context.Context, _ string, _ assetPutOptions) (assetUploadTarget, error) {
	return assetUploadTarget{}, nil
}

func (s localAssetStore) PresignGet(_ context.Context, _ string, _ assetGetOptions) (assetUploadTarget, error) {
	return assetUploadTarget{}, nil
}

func (s localAssetStore) Open(_ context.Context, objectKey string) (io.ReadCloser, error) {
	path, err := s.path(objectKey)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}

func (s localAssetStore) Delete(_ context.Context, objectKey string) error {
	path, err := s.path(objectKey)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

func (s localAssetStore) Stat(_ context.Context, objectKey string) (assetStat, error) {
	path, err := s.path(objectKey)
	if err != nil {
		return assetStat{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return assetStat{}, err
	}
	return assetStat{ByteSize: info.Size()}, nil
}

type s3AssetStore struct {
	client    *s3.Client
	presign   *s3.PresignClient
	bucket    string
	prefix    string
	expiresIn time.Duration
}

func newS3AssetStore() (s3AssetStore, error) {
	bucket := strings.TrimSpace(os.Getenv("UFO_HUB_ASSET_S3_BUCKET"))
	if bucket == "" {
		return s3AssetStore{}, fmt.Errorf("UFO_HUB_ASSET_S3_BUCKET is required")
	}
	region := strings.TrimSpace(os.Getenv("UFO_HUB_ASSET_S3_REGION"))
	if region == "" {
		region = "auto"
	}
	accessKey := strings.TrimSpace(os.Getenv("UFO_HUB_ASSET_S3_ACCESS_KEY_ID"))
	secretKey := strings.TrimSpace(os.Getenv("UFO_HUB_ASSET_S3_SECRET_ACCESS_KEY"))
	if accessKey == "" || secretKey == "" {
		return s3AssetStore{}, fmt.Errorf("UFO_HUB_ASSET_S3_ACCESS_KEY_ID and UFO_HUB_ASSET_S3_SECRET_ACCESS_KEY are required")
	}
	cfg := aws.Config{
		Region:      region,
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	}
	endpoint := strings.TrimSpace(os.Getenv("UFO_HUB_ASSET_S3_ENDPOINT"))
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		o.UsePathStyle = envBool("UFO_HUB_ASSET_S3_PATH_STYLE")
	})
	return s3AssetStore{
		client: client, presign: s3.NewPresignClient(client), bucket: bucket,
		prefix:    strings.Trim(strings.TrimSpace(os.Getenv("UFO_HUB_ASSET_S3_PREFIX")), "/"),
		expiresIn: time.Duration(envInt("UFO_HUB_ASSET_SIGNED_URL_SECONDS", 900)) * time.Second,
	}, nil
}

func (s s3AssetStore) Backend() string {
	return assetBackendS3
}

func (s s3AssetStore) key(objectKey string) string {
	objectKey = strings.TrimLeft(objectKey, "/")
	if s.prefix == "" {
		return objectKey
	}
	return s.prefix + "/" + objectKey
}

func (s s3AssetStore) Put(ctx context.Context, objectKey string, body []byte, opts assetPutOptions) error {
	_, err := s.PutReader(ctx, objectKey, bytes.NewReader(body), opts)
	return err
}

func (s s3AssetStore) PutReader(ctx context.Context, objectKey string, body io.Reader, opts assetPutOptions) (int64, error) {
	counting := &countingReader{r: body}
	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(s.key(objectKey)), Body: counting,
	}
	if opts.ByteSize > 0 {
		input.ContentLength = aws.Int64(opts.ByteSize)
	}
	if opts.ContentType != "" {
		input.ContentType = aws.String(opts.ContentType)
	}
	_, err := s.client.PutObject(ctx, input)
	return counting.n, err
}

func (s s3AssetStore) PresignUpload(ctx context.Context, objectKey string, opts assetPutOptions) (assetUploadTarget, error) {
	input := &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.key(objectKey))}
	if opts.ByteSize > 0 {
		input.ContentLength = aws.Int64(opts.ByteSize)
	}
	if opts.ContentType != "" {
		input.ContentType = aws.String(opts.ContentType)
	}
	res, err := s.presign.PresignPutObject(ctx, input, func(o *s3.PresignOptions) {
		o.Expires = s.expiresIn
	})
	if err != nil {
		return assetUploadTarget{}, err
	}
	headers := assetUploadHeaders(res.SignedHeader)
	return assetUploadTarget{Method: res.Method, URL: res.URL, Headers: headers, ExpiresAt: time.Now().UTC().Add(s.expiresIn)}, nil
}

func (s s3AssetStore) PresignGet(ctx context.Context, objectKey string, opts assetGetOptions) (assetUploadTarget, error) {
	input := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.key(objectKey))}
	if opts.ContentType != "" {
		input.ResponseContentType = aws.String(opts.ContentType)
	}
	if opts.Filename != "" {
		input.ResponseContentDisposition = aws.String(assetContentDisposition(opts.Disposition, opts.Filename))
	}
	res, err := s.presign.PresignGetObject(ctx, input, func(o *s3.PresignOptions) {
		o.Expires = s.expiresIn
	})
	if err != nil {
		return assetUploadTarget{}, err
	}
	headers := assetUploadHeaders(res.SignedHeader)
	return assetUploadTarget{Method: res.Method, URL: res.URL, Headers: headers, ExpiresAt: time.Now().UTC().Add(s.expiresIn)}, nil
}

func assetUploadHeaders(src http.Header) map[string]string {
	headers := map[string]string{}
	for k, vals := range src {
		if len(vals) == 0 || strings.EqualFold(k, "host") || strings.EqualFold(k, "content-length") {
			continue
		}
		headers[k] = vals[0]
	}
	return headers
}

func (s s3AssetStore) Open(ctx context.Context, objectKey string) (io.ReadCloser, error) {
	res, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.key(objectKey))})
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

func (s s3AssetStore) Delete(ctx context.Context, objectKey string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.key(objectKey))})
	return err
}

func (s s3AssetStore) Stat(ctx context.Context, objectKey string) (assetStat, error) {
	res, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.key(objectKey))})
	if err != nil {
		return assetStat{}, err
	}
	out := assetStat{Checksums: map[string]string{}}
	if res.ContentLength != nil {
		out.ByteSize = *res.ContentLength
	}
	if res.ChecksumCRC32 != nil {
		out.Checksums["crc32"] = normalizeChecksum("crc32", *res.ChecksumCRC32)
	}
	if res.ChecksumCRC32C != nil {
		out.Checksums["crc32c"] = normalizeChecksum("crc32c", *res.ChecksumCRC32C)
	}
	if res.ChecksumCRC64NVME != nil {
		out.Checksums["crc64nvme"] = normalizeChecksum("crc64nvme", *res.ChecksumCRC64NVME)
	}
	if res.ChecksumSHA1 != nil {
		out.Checksums["sha1"] = normalizeChecksum("sha1", *res.ChecksumSHA1)
	}
	if res.ChecksumSHA256 != nil {
		out.Checksums["sha256"] = normalizeChecksum("sha256", *res.ChecksumSHA256)
	}
	return out, nil
}

func newPublicUUID() pgtype.UUID {
	id := uuid.New()
	return pgtype.UUID{Bytes: id, Valid: true}
}

func safeFilename(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	if name == "" {
		return "asset"
	}
	base := filepath.Base(name)
	if base == "." || base == "/" {
		return "asset"
	}
	return base
}

func assetObjectKey(owner assetOwner, assetID string, createdAt time.Time) (string, error) {
	if owner.Path == "" {
		return "", fmt.Errorf("asset owner path is required")
	}
	shard := assetID[:2]
	if owner.HasDateInPath {
		return fmt.Sprintf("v1/%s/%s/%s", owner.Path, shard, assetID), nil
	}
	day := createdAt.UTC()
	return fmt.Sprintf(
		"v1/%s/%04d/%02d/%02d/%s/%s",
		owner.Path, day.Year(), day.Month(), day.Day(), shard, assetID,
	), nil
}

func blake3Hex(b []byte) string {
	sum := blake3.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func validBlake3(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func normalizeChecksum(algo string, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	lower := strings.ToLower(value)
	if expected, ok := checksumHexLengths[strings.ToLower(algo)]; ok && len(lower) == expected {
		if _, err := hex.DecodeString(lower); err == nil {
			return lower
		}
	}
	if b, err := base64.StdEncoding.DecodeString(value); err == nil {
		return hex.EncodeToString(b)
	}
	return lower
}

var checksumHexLengths = map[string]int{
	"crc32":     8,
	"crc32c":    8,
	"crc64nvme": 16,
	"md5":       32,
	"sha1":      40,
	"sha256":    64,
}

func checksumsJSON(checksums map[string]string) []byte {
	clean := map[string]string{}
	for k, v := range checksums {
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if k != "" && v != "" {
			clean[k] = v
		}
	}
	if len(clean) == 0 {
		return nil
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return nil
	}
	return b
}

func checksumMap(raw []byte) map[string]string {
	var m map[string]string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	if m == nil {
		return map[string]string{}
	}
	return m
}

func contentTypeFor(filename string, b []byte) string {
	if typ := mime.TypeByExtension(filepath.Ext(filename)); typ != "" {
		return typ
	}
	if len(b) == 0 {
		return "application/octet-stream"
	}
	return http.DetectContentType(b)
}

func assetContentTypeAllowed(contentType string) bool {
	allowedTypes := assetUploadAllowedContentTypes()
	if len(allowedTypes) == 0 {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		mediaType = strings.TrimSpace(contentType)
	}
	mediaType = strings.ToLower(mediaType)
	for _, allowed := range allowedTypes {
		if allowed == mediaType {
			return true
		}
		if strings.HasSuffix(allowed, "/*") && strings.HasPrefix(mediaType, strings.TrimSuffix(allowed, "*")) {
			return true
		}
	}
	return false
}

func assetOwnerForFleet(ctx context.Context, q *db.Queries, fleetID int64) (assetOwner, error) {
	fleet, err := q.GetFleetByID(ctx, fleetID)
	if err != nil {
		return assetOwner{}, err
	}
	return assetOwner{
		Path:    "fleets/" + uuidStr(fleet.PublicID) + "/uploads",
		FleetID: fleetID,
	}, nil
}

func assetOwnerForUser(user db.User) assetOwner {
	return assetOwner{Path: "users/" + uuidStr(user.PublicID) + "/uploads"}
}

func assetOwnerForRun(ctx context.Context, q *db.Queries, run db.Run) (assetOwner, error) {
	fleet, err := q.GetFleetByID(ctx, run.FleetID)
	if err != nil {
		return assetOwner{}, err
	}
	runID := uuidStr(run.PublicID)
	day := run.CreatedAt.Time.UTC()
	return assetOwner{
		Path: fmt.Sprintf(
			"fleets/%s/runs/%04d/%02d/%02d/%s/%s/artifacts",
			uuidStr(fleet.PublicID), day.Year(), day.Month(), day.Day(), runID[:2], runID,
		),
		FleetID:       run.FleetID,
		HasDateInPath: true,
	}, nil
}

func nullableID(id int64) pgtype.Int8 {
	return pgtype.Int8{Int64: id, Valid: id != 0}
}

func (s *Server) storeAssetBytes(ctx context.Context, q *db.Queries, owner assetOwner, filename, contentType string, body []byte, createdByUserID int64, metadata []byte) (db.Asset, error) {
	publicID := newPublicUUID()
	assetID := uuidStr(publicID)
	filename = safeFilename(filename)
	if contentType = strings.TrimSpace(contentType); contentType == "" {
		contentType = contentTypeFor(filename, body)
	}
	hash := blake3Hex(body)
	metadata = assetMetadataObject(metadata)
	objectKey, err := assetObjectKey(owner, assetID, time.Now().UTC())
	if err != nil {
		return db.Asset{}, err
	}
	opts := assetPutOptions{ContentType: contentType, ByteSize: int64(len(body))}
	if err := s.assets.Put(ctx, objectKey, body, opts); err != nil {
		return db.Asset{}, err
	}
	asset, err := q.CreateAsset(ctx, db.CreateAssetParams{
		PublicID: publicID, FleetID: nullableID(owner.FleetID),
		ObjectKey: objectKey, Filename: filename,
		ContentType: contentType, ByteSize: int64(len(body)), Checksums: checksumsJSON(map[string]string{"blake3": hash}),
		StorageBackend: s.assets.Backend(), Status: "ready", Metadata: metadata,
		CreatedBy: nullableID(createdByUserID),
	})
	if err != nil {
		_ = s.assets.Delete(ctx, objectKey)
		return db.Asset{}, err
	}
	return asset, nil
}

func (s *Server) createPendingAsset(ctx context.Context, q *db.Queries, owner assetOwner, filename, contentType string, byteSize, createdByUserID int64, metadata []byte) (db.Asset, error) {
	if byteSize <= 0 {
		return db.Asset{}, fmt.Errorf("asset size must be positive")
	}
	limit := assetUploadMaxBytes()
	if byteSize > limit {
		return db.Asset{}, fmt.Errorf("asset size exceeds %d byte limit", limit)
	}
	publicID := newPublicUUID()
	assetID := uuidStr(publicID)
	filename = safeFilename(filename)
	if contentType = strings.TrimSpace(contentType); contentType == "" {
		contentType = contentTypeFor(filename, nil)
	}
	if !assetContentTypeAllowed(contentType) {
		return db.Asset{}, fmt.Errorf("asset content type is not allowed")
	}
	metadata = assetMetadataObject(metadata)
	objectKey, err := assetObjectKey(owner, assetID, time.Now().UTC())
	if err != nil {
		return db.Asset{}, err
	}
	return q.CreateAsset(ctx, db.CreateAssetParams{
		PublicID: publicID, FleetID: nullableID(owner.FleetID),
		ObjectKey: objectKey,
		Filename:  filename, ContentType: contentType, ByteSize: byteSize, Checksums: nil,
		StorageBackend: s.assets.Backend(), Status: "pending", Metadata: metadata,
		CreatedBy: nullableID(createdByUserID),
	})
}

func (s *Server) assetUploadTarget(r *http.Request, asset db.Asset) (assetUploadTarget, error) {
	opts := assetPutOptions{ContentType: asset.ContentType, ByteSize: asset.ByteSize}
	st, err := s.assetStore(asset.StorageBackend)
	if err != nil {
		return assetUploadTarget{}, err
	}
	if asset.StorageBackend == assetBackendLocal {
		return assetUploadTarget{
			Method: "PUT",
			URL:    fmt.Sprintf("/v1/assets/%s/file", uuidStr(asset.PublicID)),
			Headers: map[string]string{
				"Content-Type": asset.ContentType,
			},
			ExpiresAt: time.Now().UTC().Add(time.Duration(envInt("UFO_HUB_ASSET_SIGNED_URL_SECONDS", 900)) * time.Second),
		}, nil
	}
	return st.PresignUpload(r.Context(), asset.ObjectKey, opts)
}

func (s *Server) verifiedAssetMetadata(ctx context.Context, asset db.Asset) (int64, []byte, []byte, error) {
	st, err := s.assetStore(asset.StorageBackend)
	if err != nil {
		return 0, nil, nil, err
	}
	stat, err := st.Stat(ctx, asset.ObjectKey)
	if err != nil {
		return 0, nil, nil, err
	}
	if stat.ByteSize != asset.ByteSize {
		return 0, nil, nil, fmt.Errorf("asset size mismatch")
	}
	if asset.StorageBackend != assetBackendLocal {
		return stat.ByteSize, checksumsJSON(stat.Checksums), asset.Metadata, nil
	}
	size, hash, err := s.hashAsset(ctx, asset)
	if err != nil {
		return 0, nil, nil, err
	}
	if size != asset.ByteSize {
		return 0, nil, nil, fmt.Errorf("asset size mismatch")
	}
	if !validBlake3(hash) {
		return 0, nil, nil, fmt.Errorf("invalid asset hash")
	}
	return size, checksumsJSON(map[string]string{"blake3": hash}), asset.Metadata, nil
}

func assetMetadataObject(base []byte) []byte {
	b, err := json.Marshal(assetMetadataMap(base))
	if err != nil {
		return []byte("{}")
	}
	return b
}

func assetMetadataWithOperation(base []byte, operationID string) []byte {
	m := assetMetadataMap(base)
	m["operation_id"] = operationID
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func assetMetadataWithUser(base []byte, userID string) []byte {
	m := assetMetadataMap(base)
	m["user_id"] = userID
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func assetMetadataWithRun(base []byte, runID string) []byte {
	m := assetMetadataMap(base)
	m["run_id"] = runID
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func assetMetadataMap(base []byte) map[string]any {
	m := map[string]any{}
	if len(base) > 0 {
		_ = json.Unmarshal(base, &m)
		if m == nil {
			m = map[string]any{}
		}
	}
	return m
}

type assetReadActor struct {
	userID       int64
	roverID      int64
	roverFleetID int64
	isUser       bool
	isRover      bool
}

func (s *Server) assetReadActor(w http.ResponseWriter, r *http.Request) (assetReadActor, bool) {
	if token := bearerToken(r); token != "" {
		if strings.Contains(token, ".") {
			user, err := s.userFromAccessToken(r.Context(), token)
			if err != nil {
				httpError(w, http.StatusUnauthorized, "invalid access token")
				return assetReadActor{}, false
			}
			return assetReadActor{userID: user.ID, isUser: true}, true
		}
		rover, ok := s.authenticateRover(w, r, token)
		if !ok {
			return assetReadActor{}, false
		}
		return assetReadActor{roverID: rover.ID, roverFleetID: rover.FleetID, isRover: true}, true
	}
	if user, ok := s.userFromCookies(w, r, true); ok {
		return assetReadActor{userID: user.ID, isUser: true}, true
	}
	httpError(w, http.StatusUnauthorized, "not authenticated")
	return assetReadActor{}, false
}

func (s *Server) assetReadAllowed(ctx context.Context, actor assetReadActor, asset db.Asset, memberCache map[int64]bool) bool {
	if actor.isRover {
		if !asset.FleetID.Valid || asset.FleetID.Int64 != actor.roverFleetID {
			return false
		}
		return s.roverCanReadAsset(ctx, actor.roverID, actor.roverFleetID, asset)
	}
	if actor.isUser {
		if asset.FleetID.Valid {
			if ok, seen := memberCache[asset.FleetID.Int64]; seen {
				return ok
			}
			isMember, err := s.q.IsMember(ctx, db.IsMemberParams{UserID: actor.userID, FleetID: asset.FleetID.Int64})
			ok := err == nil && isMember
			memberCache[asset.FleetID.Int64] = ok
			return ok
		}
		return asset.CreatedBy.Valid && asset.CreatedBy.Int64 == actor.userID
	}
	return false
}

func (s *Server) requireAssetReadAccess(w http.ResponseWriter, r *http.Request, asset db.Asset) bool {
	actor, ok := s.assetReadActor(w, r)
	if !ok {
		return false
	}
	if s.assetReadAllowed(r.Context(), actor, asset, map[int64]bool{}) {
		return true
	}
	httpError(w, http.StatusForbidden, "not allowed to access this asset")
	return false
}

func (s *Server) assetWriteAllowed(ctx context.Context, actor assetWriteActor, asset db.Asset) bool {
	if actor.isUser {
		if asset.CreatedBy.Valid {
			return asset.CreatedBy.Int64 == actor.user.ID
		}
		if asset.FleetID.Valid {
			ok, err := s.q.IsMember(ctx, db.IsMemberParams{UserID: actor.user.ID, FleetID: asset.FleetID.Int64})
			return err == nil && ok
		}
	}
	if actor.isRover && asset.FleetID.Valid && asset.FleetID.Int64 == actor.rover.FleetID {
		runID, _ := assetMetadataMap(asset.Metadata)["run_id"].(string)
		pid, ok := parseUUID(runID)
		if !ok {
			return false
		}
		_, err := s.q.GetRunForRover(ctx, db.GetRunForRoverParams{
			PublicID: pid, FleetID: actor.rover.FleetID, RoverID: pgtype.Int8{Int64: actor.rover.ID, Valid: true},
		})
		return err == nil
	}
	return false
}

func (s *Server) requireAssetWriteAccess(w http.ResponseWriter, r *http.Request, actor assetWriteActor, asset db.Asset) bool {
	if s.assetWriteAllowed(r.Context(), actor, asset) {
		return true
	}
	httpError(w, http.StatusForbidden, "not allowed to modify this asset")
	return false
}

func (s *Server) readAssetBytes(ctx context.Context, asset db.Asset) ([]byte, error) {
	st, err := s.assetStore(asset.StorageBackend)
	if err != nil {
		return nil, err
	}
	f, err := st.Open(ctx, asset.ObjectKey)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if asset.Status == "ready" && checksumMap(asset.Checksums)["blake3"] == "" {
		_ = s.q.MergeAssetChecksums(ctx, db.MergeAssetChecksumsParams{ID: asset.ID, Checksums: checksumsJSON(map[string]string{"blake3": blake3Hex(b)})})
	}
	return b, nil
}

func (s *Server) hashAsset(ctx context.Context, asset db.Asset) (int64, string, error) {
	st, err := s.assetStore(asset.StorageBackend)
	if err != nil {
		return 0, "", err
	}
	f, err := st.Open(ctx, asset.ObjectKey)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := blake3.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return n, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Server) removeAsset(asset db.Asset) {
	st, err := s.assetStore(asset.StorageBackend)
	if err != nil {
		return
	}
	_ = st.Delete(context.Background(), asset.ObjectKey)
}

func (s *Server) openAsset(r *http.Request, asset db.Asset) (io.ReadCloser, error) {
	st, err := s.assetStore(asset.StorageBackend)
	if err != nil {
		return nil, err
	}
	return st.Open(r.Context(), asset.ObjectKey)
}

type assetDTO struct {
	ID          string          `json:"id"`
	Filename    string          `json:"filename"`
	ContentType string          `json:"content_type"`
	ByteSize    int64           `json:"byte_size"`
	Checksums   json.RawMessage `json:"checksums,omitempty"`
	URL         string          `json:"url"`
	Metadata    json.RawMessage `json:"metadata"`
	CreatedBy   *string         `json:"created_by"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

func assetFileURL(assetID string) string {
	return "/v1/assets/" + assetID + "/file"
}

func assetResponseDisposition(r *http.Request) string {
	if strings.EqualFold(r.URL.Query().Get("disposition"), "inline") {
		return "inline"
	}
	return "attachment"
}

func inlineSafeContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		mediaType = strings.TrimSpace(contentType)
	}
	mediaType = strings.ToLower(mediaType)
	if mediaType == "image/svg+xml" {
		return false
	}
	switch {
	case mediaType == "application/pdf", mediaType == "application/json",
		strings.HasSuffix(mediaType, "+json"), mediaType == "text/plain",
		mediaType == "text/markdown", mediaType == "text/csv",
		mediaType == "text/tab-separated-values", mediaType == "text/x-python",
		mediaType == "text/x-shellscript", mediaType == "text/x-go",
		mediaType == "text/x-rust", mediaType == "text/x-sql":
		return true
	case strings.HasPrefix(mediaType, "image/"),
		strings.HasPrefix(mediaType, "audio/"),
		strings.HasPrefix(mediaType, "video/"):
		return true
	}
	return false
}

func inlineSafeAsset(asset db.Asset) bool {
	if inlineSafeContentType(asset.ContentType) {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(asset.ContentType))
	if err != nil {
		mediaType = strings.TrimSpace(asset.ContentType)
	}
	mediaType = strings.ToLower(mediaType)
	if mediaType != "" && mediaType != "application/octet-stream" {
		return false
	}
	ext := strings.ToLower(filepath.Ext(asset.Filename))
	name := strings.ToLower(filepath.Base(asset.Filename))
	switch ext {
	case ".c", ".cc", ".cfg", ".conf", ".cpp", ".cs", ".css", ".csv", ".dart",
		".diff", ".env", ".go", ".h", ".hpp", ".html", ".ini", ".java", ".js",
		".jsx", ".json", ".jsonl", ".kt", ".kts", ".log", ".lua", ".md", ".patch",
		".php", ".pl", ".py", ".r", ".rb", ".rs", ".scala", ".sh", ".sql", ".swift",
		".svelte", ".toml", ".ts", ".tsx", ".txt", ".vue", ".xml", ".yaml", ".yml":
		return true
	}
	switch name {
	case "dockerfile", "gemfile", "justfile", "makefile", "rakefile":
		return true
	}
	return false
}

func assetContentDisposition(disposition, filename string) string {
	if disposition != "inline" {
		disposition = "attachment"
	}
	return mime.FormatMediaType(disposition, map[string]string{"filename": filename})
}

func assetDTOFromAsset(asset db.Asset, creators map[int64]string) assetDTO {
	id := uuidStr(asset.PublicID)
	d := assetDTO{
		ID:          id,
		Filename:    asset.Filename,
		ContentType: asset.ContentType,
		ByteSize:    asset.ByteSize, Checksums: asset.Checksums, URL: assetFileURL(id),
		Metadata: metadataJSON(asset.Metadata),
	}
	if asset.CreatedBy.Valid {
		d.CreatedBy = strPtr(creators[asset.CreatedBy.Int64])
	}
	d.CreatedAt = asset.CreatedAt.Time
	d.UpdatedAt = asset.UpdatedAt.Time
	return d
}

func (s *Server) assetDTOs(ctx context.Context, assets []db.Asset) []assetDTO {
	creatorIDs := make([]int64, 0, len(assets))
	for _, asset := range assets {
		if asset.CreatedBy.Valid {
			creatorIDs = append(creatorIDs, asset.CreatedBy.Int64)
		}
	}
	creatorMap := s.mapUsers(ctx, creatorIDs)
	out := make([]assetDTO, len(assets))
	for i, asset := range assets {
		out[i] = assetDTOFromAsset(asset, creatorMap)
	}
	return out
}

func (s *Server) assetDTO(ctx context.Context, asset db.Asset) assetDTO {
	return s.assetDTOs(ctx, []db.Asset{asset})[0]
}

func assetPublicIDsFromStrings(ids []string) ([]pgtype.UUID, bool) {
	out := make([]pgtype.UUID, 0, len(ids))
	seen := map[string]struct{}{}
	for _, raw := range ids {
		id := strings.ToLower(strings.TrimSpace(raw))
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		pid, ok := parseUUID(id)
		if !ok {
			return nil, false
		}
		seen[id] = struct{}{}
		out = append(out, pid)
	}
	return out, true
}

func assetPublicIDsFromText(text string) []pgtype.UUID {
	matches := assetFileLinkRE.FindAllStringSubmatch(text, -1)
	ids := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) >= 2 {
			ids = append(ids, match[1])
		}
	}
	out, _ := assetPublicIDsFromStrings(ids)
	return out
}

func orderAssetsByPublicIDs(ids []pgtype.UUID, assets []db.Asset) []db.Asset {
	byID := make(map[string]db.Asset, len(assets))
	for _, asset := range assets {
		byID[uuidStr(asset.PublicID)] = asset
	}
	out := make([]db.Asset, 0, len(ids))
	for _, id := range ids {
		if asset, ok := byID[uuidStr(id)]; ok {
			out = append(out, asset)
		}
	}
	return out
}

func mergeAssetRows(groups ...[]db.Asset) []db.Asset {
	seen := make(map[string]struct{})
	out := make([]db.Asset, 0)
	for _, group := range groups {
		for _, asset := range group {
			id := uuidStr(asset.PublicID)
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, asset)
		}
	}
	return out
}

func (s *Server) assetsForText(ctx context.Context, q *db.Queries, fleetID int64, text string) []db.Asset {
	ids := assetPublicIDsFromText(text)
	if len(ids) == 0 {
		return nil
	}
	assets, err := q.ListAssetsByPublicIDs(ctx, db.ListAssetsByPublicIDsParams{
		FleetID:        pgtype.Int8{Int64: fleetID, Valid: true},
		AssetPublicIds: ids,
	})
	if err != nil {
		return nil
	}
	return orderAssetsByPublicIDs(ids, assets)
}

func (s *Server) operationAssets(ctx context.Context, q *db.Queries, op db.Operation, text string) ([]db.Asset, error) {
	referenced := s.assetsForText(ctx, q, op.FleetID, text)
	contextual, err := q.ListReadyAssetsByOperationID(ctx, db.ListReadyAssetsByOperationIDParams{
		FleetID: pgtype.Int8{Int64: op.FleetID, Valid: true}, OperationID: uuidStr(op.PublicID),
	})
	if err != nil {
		return nil, err
	}
	groups := [][]db.Asset{referenced, contextual}
	if !op.MainOperationID.Valid {
		subOperations, err := q.ListSubOperations(ctx, pgtype.Int8{Int64: op.ID, Valid: true})
		if err != nil {
			return nil, err
		}
		for _, subOperation := range subOperations {
			assets, err := q.ListReadyAssetsByOperationID(ctx, db.ListReadyAssetsByOperationIDParams{
				FleetID: pgtype.Int8{Int64: op.FleetID, Valid: true}, OperationID: uuidStr(subOperation.PublicID),
			})
			if err != nil {
				return nil, err
			}
			groups = append(groups, assets)
		}
	}
	return mergeAssetRows(groups...), nil
}

func (s *Server) roverCanReadAsset(ctx context.Context, roverID, fleetID int64, asset db.Asset) bool {
	runs, err := s.q.ListActiveRunOperationsForRover(ctx, db.ListActiveRunOperationsForRoverParams{
		RoverID: pgtype.Int8{Int64: roverID, Valid: true},
		FleetID: fleetID,
	})
	if err != nil {
		return false
	}
	target := uuidStr(asset.PublicID)
	for _, run := range runs {
		op, err := s.q.GetOperation(ctx, db.GetOperationParams{ID: run.OperationID, FleetID: fleetID})
		if err != nil {
			continue
		}
		assets, err := s.operationAssets(ctx, s.q, op, s.operationAssetReferenceText(ctx, s.q, op, run.Command))
		if err != nil {
			continue
		}
		for _, a := range assets {
			if uuidStr(a.PublicID) == target {
				return true
			}
		}
	}
	return false
}

func (s *Server) getAssetFile(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	asset, err := s.q.GetAssetByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "asset not found")
		return
	}
	if !s.requireAssetReadAccess(w, r, asset) {
		return
	}
	s.serveAssetFile(w, r, asset)
}

func (s *Server) serveAssetFile(w http.ResponseWriter, r *http.Request, asset db.Asset) {
	disposition := assetResponseDisposition(r)
	if disposition == "inline" && !inlineSafeAsset(asset) {
		disposition = "attachment"
	}
	if asset.StorageBackend != assetBackendLocal {
		st, err := s.assetStore(asset.StorageBackend)
		if err != nil {
			serverError(w, err)
			return
		}
		target, err := st.PresignGet(r.Context(), asset.ObjectKey, assetGetOptions{Filename: asset.Filename, ContentType: asset.ContentType, Disposition: disposition})
		if err != nil {
			serverError(w, err)
			return
		}
		http.Redirect(w, r, target.URL, http.StatusFound)
		return
	}
	f, err := s.openAsset(r, asset)
	if err != nil {
		httpError(w, http.StatusNotFound, "asset not found")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", asset.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(asset.ByteSize, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if blake3 := checksumMap(asset.Checksums)["blake3"]; blake3 != "" {
		w.Header().Set("X-UFO-Blake3", blake3)
	}
	w.Header().Set("Content-Disposition", assetContentDisposition(disposition, asset.Filename))
	if _, err := io.Copy(w, f); err != nil {
		return
	}
}

func (s *Server) getAsset(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	asset, err := s.q.GetAssetByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "asset not found")
		return
	}
	if !s.requireAssetReadAccess(w, r, asset) {
		return
	}
	writeJSON(w, http.StatusOK, s.assetDTO(r.Context(), asset))
}

type resolveAssetsReq struct {
	IDs []string `json:"ids"`
}

func (s *Server) resolveAssets(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.assetReadActor(w, r)
	if !ok {
		return
	}
	var req resolveAssetsReq
	if !readJSON(w, r, &req) {
		return
	}
	if len(req.IDs) > assetResolveMaxIDs {
		httpError(w, http.StatusBadRequest, "too many asset ids")
		return
	}
	ids, ok := assetPublicIDsFromStrings(req.IDs)
	if !ok {
		httpError(w, http.StatusBadRequest, "invalid asset id")
		return
	}
	if len(ids) == 0 {
		writeJSON(w, http.StatusOK, []assetDTO{})
		return
	}
	assets, err := s.q.ListReadyAssetsByPublicIDs(r.Context(), ids)
	if err != nil {
		serverError(w, err)
		return
	}
	memberCache := map[int64]bool{}
	filtered := make([]db.Asset, 0, len(assets))
	for _, asset := range orderAssetsByPublicIDs(ids, assets) {
		if s.assetReadAllowed(r.Context(), actor, asset, memberCache) {
			filtered = append(filtered, asset)
		}
	}
	writeJSON(w, http.StatusOK, s.assetDTOs(r.Context(), filtered))
}

func (s *Server) listAssets(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(r.URL.Query().Get("operation_id")) != "" {
		op, ok := s.operationFromQuery(w, r, "operation_id")
		if !ok {
			return
		}
		text := s.operationAssetReferenceText(r.Context(), s.q, op, "")
		assets, err := s.operationAssets(r.Context(), s.q, op, text)
		if err != nil {
			serverError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.assetDTOs(r.Context(), assets))
		return
	}
	fleetIDs, ok := s.fleetIDsFromQuery(w, r)
	if !ok {
		return
	}
	limit := int32(100)
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			httpError(w, http.StatusBadRequest, "limit must be 1..500")
			return
		}
		limit = int32(n)
	}
	var assets []db.Asset
	for _, wid := range fleetIDs {
		rows, err := s.q.ListReadyAssetsByFleet(r.Context(), db.ListReadyAssetsByFleetParams{
			FleetID: pgtype.Int8{Int64: wid, Valid: true}, Limit: limit,
		})
		if err != nil {
			serverError(w, err)
			return
		}
		assets = append(assets, rows...)
	}
	if len(fleetIDs) > 1 {
		sort.Slice(assets, func(i, j int) bool { return assets[i].ID > assets[j].ID })
		if int32(len(assets)) > limit {
			assets = assets[:limit]
		}
	}
	writeJSON(w, http.StatusOK, s.assetDTOs(r.Context(), assets))
}

type assetUploadContextReq struct {
	OperationID string `json:"operation_id"`
	RunID       string `json:"run_id"`
	UserID      string `json:"user_id"`
}

type createAssetReq struct {
	FleetID     string                `json:"fleet_id"`
	Context     assetUploadContextReq `json:"context"`
	Filename    string                `json:"filename"`
	ContentType string                `json:"content_type"`
	ByteSize    int64                 `json:"byte_size"`
}

type assetUploadIntentDTO struct {
	AssetID   string            `json:"asset_id"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt time.Time         `json:"expires_at"`
}

type assetWriteActor struct {
	user    db.User
	rover   roverCtx
	isUser  bool
	isRover bool
}

func (s *Server) assetWriteActor(w http.ResponseWriter, r *http.Request) (assetWriteActor, bool) {
	if token := bearerToken(r); token != "" {
		if strings.Contains(token, ".") {
			user, err := s.userFromAccessToken(r.Context(), token)
			if err != nil {
				httpError(w, http.StatusUnauthorized, "invalid access token")
				return assetWriteActor{}, false
			}
			return assetWriteActor{user: user, isUser: true}, true
		}
		rv, ok := s.authenticateRover(w, r, token)
		if !ok {
			return assetWriteActor{}, false
		}
		return assetWriteActor{rover: roverCtx{ID: rv.ID, FleetID: rv.FleetID, Name: rv.Name, Tags: unionTags(rv.AutoTags, rv.Tags)}, isRover: true}, true
	}
	if user, ok := s.userFromCookies(w, r, true); ok {
		return assetWriteActor{user: user, isUser: true}, true
	}
	httpError(w, http.StatusUnauthorized, "not authenticated")
	return assetWriteActor{}, false
}

func (s *Server) requireUserAssetMember(w http.ResponseWriter, r *http.Request, userID, fleetID int64) bool {
	ok, err := s.q.IsMember(r.Context(), db.IsMemberParams{UserID: userID, FleetID: fleetID})
	if err != nil {
		serverError(w, err)
		return false
	}
	if !ok {
		httpError(w, http.StatusForbidden, "not a member of this fleet")
		return false
	}
	return true
}

func (s *Server) resolveAssetUploadContext(w http.ResponseWriter, r *http.Request, actor assetWriteActor, req createAssetReq) (assetOwner, []byte, bool) {
	hasOperation := strings.TrimSpace(req.Context.OperationID) != ""
	hasRun := strings.TrimSpace(req.Context.RunID) != ""
	hasUser := strings.TrimSpace(req.Context.UserID) != ""
	if boolCount(hasOperation, hasRun, hasUser) > 1 {
		httpError(w, http.StatusBadRequest, "asset context must identify one resource")
		return assetOwner{}, nil, false
	}
	if hasOperation {
		if !actor.isUser {
			httpError(w, http.StatusForbidden, "context.operation_id requires user auth")
			return assetOwner{}, nil, false
		}
		pid, ok := parseUUID(strings.TrimSpace(req.Context.OperationID))
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid context.operation_id")
			return assetOwner{}, nil, false
		}
		op, err := s.q.GetOperationByPublicID(r.Context(), pid)
		if err != nil {
			httpError(w, http.StatusNotFound, "operation not found")
			return assetOwner{}, nil, false
		}
		if !s.requireUserAssetMember(w, r, actor.user.ID, op.FleetID) {
			return assetOwner{}, nil, false
		}
		if strings.TrimSpace(req.FleetID) != "" {
			fleetID, ok := s.resolveFleetPublicIDForUser(w, r, req.FleetID, actor.user.ID)
			if !ok {
				return assetOwner{}, nil, false
			}
			if fleetID != op.FleetID {
				httpError(w, http.StatusBadRequest, "fleet_id does not match context.operation_id")
				return assetOwner{}, nil, false
			}
		}
		owner, err := assetOwnerForFleet(r.Context(), s.q, op.FleetID)
		if err != nil {
			serverError(w, err)
			return assetOwner{}, nil, false
		}
		return owner, assetMetadataWithOperation(nil, uuidStr(op.PublicID)), true
	}
	if hasRun {
		if !actor.isRover {
			httpError(w, http.StatusForbidden, "context.run_id requires rover auth")
			return assetOwner{}, nil, false
		}
		pid, ok := parseUUID(strings.TrimSpace(req.Context.RunID))
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid context.run_id")
			return assetOwner{}, nil, false
		}
		run, err := s.q.GetRunForRover(r.Context(), db.GetRunForRoverParams{
			PublicID: pid, FleetID: actor.rover.FleetID, RoverID: pgtype.Int8{Int64: actor.rover.ID, Valid: true},
		})
		if err != nil {
			httpError(w, http.StatusNotFound, "run not found")
			return assetOwner{}, nil, false
		}
		op, err := s.q.GetOperation(r.Context(), db.GetOperationParams{ID: run.OperationID, FleetID: run.FleetID})
		if err != nil {
			serverError(w, err)
			return assetOwner{}, nil, false
		}
		owner, err := assetOwnerForRun(r.Context(), s.q, run)
		if err != nil {
			serverError(w, err)
			return assetOwner{}, nil, false
		}
		metadata := assetMetadataWithRun(assetMetadataWithOperation(nil, uuidStr(op.PublicID)), uuidStr(run.PublicID))
		return owner, metadata, true
	}
	if hasUser {
		if !actor.isUser {
			httpError(w, http.StatusForbidden, "context.user_id requires user auth")
			return assetOwner{}, nil, false
		}
		if strings.TrimSpace(req.FleetID) != "" {
			httpError(w, http.StatusBadRequest, "fleet_id is not valid for context.user_id")
			return assetOwner{}, nil, false
		}
		pid, ok := parseUUID(strings.TrimSpace(req.Context.UserID))
		if !ok {
			httpError(w, http.StatusBadRequest, "invalid context.user_id")
			return assetOwner{}, nil, false
		}
		if uuidStr(pid) != uuidStr(actor.user.PublicID) {
			httpError(w, http.StatusForbidden, "not allowed to upload for this user")
			return assetOwner{}, nil, false
		}
		userID := uuidStr(actor.user.PublicID)
		return assetOwnerForUser(actor.user), assetMetadataWithUser(nil, userID), true
	}
	if !actor.isUser {
		httpError(w, http.StatusBadRequest, "context.run_id is required")
		return assetOwner{}, nil, false
	}
	fleetID, ok := s.resolveFleetPublicIDForUser(w, r, req.FleetID, actor.user.ID)
	if !ok {
		return assetOwner{}, nil, false
	}
	owner, err := assetOwnerForFleet(r.Context(), s.q, fleetID)
	if err != nil {
		serverError(w, err)
		return assetOwner{}, nil, false
	}
	return owner, nil, true
}

func (s *Server) createAsset(w http.ResponseWriter, r *http.Request) {
	var req createAssetReq
	if !readJSON(w, r, &req) {
		return
	}
	actor, ok := s.assetWriteActor(w, r)
	if !ok {
		return
	}
	owner, metadata, ok := s.resolveAssetUploadContext(w, r, actor, req)
	if !ok {
		return
	}
	createdBy := int64(0)
	if actor.isUser {
		createdBy = actor.user.ID
	}
	asset, err := s.createPendingAsset(r.Context(), s.q, owner, req.Filename, req.ContentType, req.ByteSize, createdBy, metadata)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	uploadTarget, err := s.assetUploadTarget(r, asset)
	if err != nil {
		_ = s.q.DeletePendingAsset(r.Context(), asset.ID)
		s.removeAsset(asset)
		serverError(w, err)
		return
	}
	headers := uploadTarget.Headers
	if headers == nil {
		headers = map[string]string{}
	}
	writeJSON(w, http.StatusCreated, assetUploadIntentDTO{
		AssetID: uuidStr(asset.PublicID), Method: uploadTarget.Method, URL: uploadTarget.URL,
		Headers: headers, ExpiresAt: uploadTarget.ExpiresAt,
	})
}

func (s *Server) putAssetFile(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	actor, ok := s.assetWriteActor(w, r)
	if !ok {
		return
	}
	asset, err := s.q.GetPendingAssetByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "asset not found")
		return
	}
	if !s.requireAssetWriteAccess(w, r, actor, asset) {
		return
	}
	if asset.Status != "pending" || asset.StorageBackend != assetBackendLocal {
		httpError(w, http.StatusBadRequest, "asset is not uploadable")
		return
	}
	st, err := s.assetStore(asset.StorageBackend)
	if err != nil {
		serverError(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, asset.ByteSize+1)
	n, err := st.PutReader(r.Context(), asset.ObjectKey, r.Body, assetPutOptions{ContentType: asset.ContentType, ByteSize: asset.ByteSize})
	if err != nil {
		_ = st.Delete(r.Context(), asset.ObjectKey)
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			httpError(w, http.StatusBadRequest, "upload size mismatch")
			return
		}
		serverError(w, err)
		return
	}
	if n != asset.ByteSize {
		_ = st.Delete(r.Context(), asset.ObjectKey)
		httpError(w, http.StatusBadRequest, "upload size mismatch")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type patchAssetReq struct {
	Status string `json:"status"`
}

func (s *Server) patchAsset(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var req patchAssetReq
	if !readJSON(w, r, &req) {
		return
	}
	if req.Status != "ready" {
		httpError(w, http.StatusBadRequest, "unsupported asset patch")
		return
	}
	actor, ok := s.assetWriteActor(w, r)
	if !ok {
		return
	}
	asset, err := s.q.GetPendingAssetByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "asset not found")
		return
	}
	if !s.requireAssetWriteAccess(w, r, actor, asset) {
		return
	}
	if asset.Status != "pending" {
		httpError(w, http.StatusBadRequest, "asset is not pending")
		return
	}
	size, checksums, metadata, err := s.verifiedAssetMetadata(r.Context(), asset)
	if err != nil {
		httpError(w, http.StatusBadRequest, "upload verification failed")
		return
	}
	ready, err := s.q.SetAssetReady(r.Context(), db.SetAssetReadyParams{ID: asset.ID, ByteSize: size, Checksums: checksums, Metadata: metadata})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.assetDTO(r.Context(), ready))
}

func (s *Server) deleteAsset(w http.ResponseWriter, r *http.Request) {
	pid, ok := pathUUID(w, r)
	if !ok {
		return
	}
	actor, ok := s.assetWriteActor(w, r)
	if !ok {
		return
	}
	asset, err := s.q.GetAssetForDeleteByPublicID(r.Context(), pid)
	if err != nil {
		httpError(w, http.StatusNotFound, "asset not found")
		return
	}
	if !s.requireAssetWriteAccess(w, r, actor, asset) {
		return
	}
	if err := s.q.DeleteAsset(r.Context(), asset.ID); err != nil {
		serverError(w, err)
		return
	}
	if asset.StorageBackend == assetBackendLocal {
		s.removeAsset(asset)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) assetForID(ctx context.Context, assetID int64) (db.Asset, bool) {
	asset, err := s.q.GetAssetByID(ctx, assetID)
	if err != nil {
		return db.Asset{}, false
	}
	return asset, true
}

func (s *Server) artifactContent(ctx context.Context, a db.Artifact) string {
	if a.Content != "" || !a.AssetID.Valid {
		return a.Content
	}
	if a.AssetID.Valid {
		if asset, ok := s.assetForID(ctx, a.AssetID.Int64); ok && asset.ByteSize <= maxLargeBody {
			if b, err := s.readAssetBytes(ctx, asset); err == nil {
				return string(b)
			}
		}
	}
	return ""
}
