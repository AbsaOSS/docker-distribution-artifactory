// Package s3 provides a storagedriver.StorageDriver implementation to
// store blobs in Amazon S3 cloud storage.
//
// This package leverages the official aws client library for interfacing with
// S3.
//
// Because S3 is a key, value store the Stat call does not support last modification
// time for directories (directories are an abstraction for key, value stores)
//
// Keep in mind that S3 guarantees only read-after-write consistency for new
// objects, but no read-after-update or list-after-write consistency.
package s3artifactory

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"

	dcontext "github.com/distribution/distribution/v3/context"
	storagedriver "github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/distribution/distribution/v3/registry/storage/driver/base"
	"github.com/distribution/distribution/v3/registry/storage/driver/factory"
)

const driverName = "s3awsartifactory"

// minChunkSize defines the minimum multipart upload chunk size
// S3 API requires multipart upload chunks to be at least 5MB
const minChunkSize = 5 << 20

// maxChunkSize defines the maximum multipart upload chunk size allowed by S3.
const maxChunkSize = 5 << 30

const defaultChunkSize = 2 * minChunkSize

const (
	// defaultMultipartCopyChunkSize defines the default chunk size for all
	// but the last Upload Part - Copy operation of a multipart copy.
	// Empirically, 32 MB is optimal.
	defaultMultipartCopyChunkSize = 32 << 20

	// defaultMultipartCopyMaxConcurrency defines the default maximum number
	// of concurrent Upload Part - Copy operations for a multipart copy.
	defaultMultipartCopyMaxConcurrency = 100

	// defaultMultipartCopyThresholdSize defines the default object size
	// above which multipart copy will be used. (PUT Object - Copy is used
	// for objects at or below this size.)  Empirically, 32 MB is optimal.
	defaultMultipartCopyThresholdSize = 32 << 20
)

// listMax is the largest amount of objects you can request from S3 in a list call
const listMax = 1000

// noStorageClass defines the value to be used if storage class is not supported by the S3 endpoint
const noStorageClass = "NONE"

// s3StorageClasses lists all compatible (instant retrieval) S3 storage classes
var s3StorageClasses = []string{
	noStorageClass,
	s3.StorageClassStandard,
	s3.StorageClassReducedRedundancy,
	s3.StorageClassStandardIa,
	s3.StorageClassOnezoneIa,
	s3.StorageClassIntelligentTiering,
	s3.StorageClassOutposts,
	s3.StorageClassGlacierIr,
}

// validRegions maps known s3 region identifiers to region descriptors
var validRegions = map[string]struct{}{}

// validObjectACLs contains known s3 object Acls
var validObjectACLs = map[string]struct{}{}

// DriverParameters A struct that encapsulates all of the driver parameters after all values have been set
type DriverParameters struct {
	AccessKey                   string
	SecretKey                   string
	Bucket                      string
	Region                      string
	RegionEndpoint              string
	ForcePathStyle              bool
	Encrypt                     bool
	KeyID                       string
	Secure                      bool
	SkipVerify                  bool
	V4Auth                      bool
	ChunkSize                   int64
	MultipartCopyChunkSize      int64
	MultipartCopyMaxConcurrency int64
	MultipartCopyThresholdSize  int64
	MultipartCombineSmallPart   bool
	RootDirectory               string
	StorageClass                string
	UserAgent                   string
	ObjectACL                   string
	SessionToken                string
	UseDualStack                bool
	Accelerate                  bool
	MetadataPath                string
}

func init() {
	partitions := endpoints.DefaultPartitions()
	for _, p := range partitions {
		for region := range p.Regions() {
			validRegions[region] = struct{}{}
		}
	}

	for _, objectACL := range []string{
		s3.ObjectCannedACLPrivate,
		s3.ObjectCannedACLPublicRead,
		s3.ObjectCannedACLPublicReadWrite,
		s3.ObjectCannedACLAuthenticatedRead,
		s3.ObjectCannedACLAwsExecRead,
		s3.ObjectCannedACLBucketOwnerRead,
		s3.ObjectCannedACLBucketOwnerFullControl,
	} {
		validObjectACLs[objectACL] = struct{}{}
	}

	// Register this as the default s3 driver in addition to s3aws
	factory.Register("s3", &s3DriverFactory{})
	factory.Register(driverName, &s3DriverFactory{})
}

// s3DriverFactory implements the factory.StorageDriverFactory interface
type s3DriverFactory struct{}

func (factory *s3DriverFactory) Create(parameters map[string]interface{}) (storagedriver.StorageDriver, error) {
	return FromParameters(parameters)
}

var _ storagedriver.StorageDriver = &driver{}

type driver struct {
	S3                          *s3.S3
	Bucket                      string
	ChunkSize                   int64
	Encrypt                     bool
	KeyID                       string
	MultipartCopyChunkSize      int64
	MultipartCopyMaxConcurrency int64
	MultipartCopyThresholdSize  int64
	MultipartCombineSmallPart   bool
	RootDirectory               string
	StorageClass                string
	ObjectACL                   string
	ArtifactoryMetadata         map[string]string
}

type baseEmbed struct {
	base.Base
}

// Driver is a storagedriver.StorageDriver implementation backed by Amazon S3
// Objects are stored at absolute keys in the provided bucket.
type Driver struct {
	baseEmbed
}

// FromParameters constructs a new Driver with a given parameters map
// Required parameters:
// - accesskey
// - secretkey
// - region
// - bucket
// - encrypt
func FromParameters(parameters map[string]interface{}) (*Driver, error) {
	// Providing no values for these is valid in case the user is authenticating
	// with an IAM on an ec2 instance (in which case the instance credentials will
	// be summoned when GetAuth is called)
	accessKey := parameters["accesskey"]
	if accessKey == nil {
		accessKey = ""
	}
	secretKey := parameters["secretkey"]
	if secretKey == nil {
		secretKey = ""
	}

	regionEndpoint := parameters["regionendpoint"]
	if regionEndpoint == nil {
		regionEndpoint = ""
	}

	forcePathStyleBool := true
	forcePathStyle := parameters["forcepathstyle"]
	switch forcePathStyle := forcePathStyle.(type) {
	case string:
		b, err := strconv.ParseBool(forcePathStyle)
		if err != nil {
			return nil, fmt.Errorf("the forcePathStyle parameter should be a boolean")
		}
		forcePathStyleBool = b
	case bool:
		forcePathStyleBool = forcePathStyle
	case nil:
		// do nothing
	default:
		return nil, fmt.Errorf("the forcePathStyle parameter should be a boolean")
	}

	regionName := parameters["region"]
	if regionName == nil || fmt.Sprint(regionName) == "" {
		return nil, fmt.Errorf("no region parameter provided")
	}
	region := fmt.Sprint(regionName)
	// Don't check the region value if a custom endpoint is provided.
	if regionEndpoint == "" {
		if _, ok := validRegions[region]; !ok {
			return nil, fmt.Errorf("invalid region provided: %v", region)
		}
	}

	bucket := parameters["bucket"]
	if bucket == nil || fmt.Sprint(bucket) == "" {
		return nil, fmt.Errorf("no bucket parameter provided")
	}

	encryptBool := false
	encrypt := parameters["encrypt"]
	switch encrypt := encrypt.(type) {
	case string:
		b, err := strconv.ParseBool(encrypt)
		if err != nil {
			return nil, fmt.Errorf("the encrypt parameter should be a boolean")
		}
		encryptBool = b
	case bool:
		encryptBool = encrypt
	case nil:
		// do nothing
	default:
		return nil, fmt.Errorf("the encrypt parameter should be a boolean")
	}

	secureBool := true
	secure := parameters["secure"]
	switch secure := secure.(type) {
	case string:
		b, err := strconv.ParseBool(secure)
		if err != nil {
			return nil, fmt.Errorf("the secure parameter should be a boolean")
		}
		secureBool = b
	case bool:
		secureBool = secure
	case nil:
		// do nothing
	default:
		return nil, fmt.Errorf("the secure parameter should be a boolean")
	}

	skipVerifyBool := false
	skipVerify := parameters["skipverify"]
	switch skipVerify := skipVerify.(type) {
	case string:
		b, err := strconv.ParseBool(skipVerify)
		if err != nil {
			return nil, fmt.Errorf("the skipVerify parameter should be a boolean")
		}
		skipVerifyBool = b
	case bool:
		skipVerifyBool = skipVerify
	case nil:
		// do nothing
	default:
		return nil, fmt.Errorf("the skipVerify parameter should be a boolean")
	}

	v4Bool := true
	v4auth := parameters["v4auth"]
	switch v4auth := v4auth.(type) {
	case string:
		b, err := strconv.ParseBool(v4auth)
		if err != nil {
			return nil, fmt.Errorf("the v4auth parameter should be a boolean")
		}
		v4Bool = b
	case bool:
		v4Bool = v4auth
	case nil:
		// do nothing
	default:
		return nil, fmt.Errorf("the v4auth parameter should be a boolean")
	}

	keyID := parameters["keyid"]
	if keyID == nil {
		keyID = ""
	}

	chunkSize, err := getParameterAsInt64(parameters, "chunksize", defaultChunkSize, minChunkSize, maxChunkSize)
	if err != nil {
		return nil, err
	}

	multipartCopyChunkSize, err := getParameterAsInt64(parameters, "multipartcopychunksize", defaultMultipartCopyChunkSize, minChunkSize, maxChunkSize)
	if err != nil {
		return nil, err
	}

	multipartCopyMaxConcurrency, err := getParameterAsInt64(parameters, "multipartcopymaxconcurrency", defaultMultipartCopyMaxConcurrency, 1, math.MaxInt64)
	if err != nil {
		return nil, err
	}

	multipartCopyThresholdSize, err := getParameterAsInt64(parameters, "multipartcopythresholdsize", defaultMultipartCopyThresholdSize, 0, maxChunkSize)
	if err != nil {
		return nil, err
	}

	rootDirectory := parameters["rootdirectory"]
	if rootDirectory == nil {
		rootDirectory = ""
	}

	storageClass := s3.StorageClassStandard
	storageClassParam := parameters["storageclass"]
	if storageClassParam != nil {
		storageClassString, ok := storageClassParam.(string)
		if !ok {
			return nil, fmt.Errorf(
				"the storageclass parameter must be one of %v, %v invalid",
				s3StorageClasses,
				storageClassParam,
			)
		}
		// All valid storage class parameters are UPPERCASE, so be a bit more flexible here
		storageClassString = strings.ToUpper(storageClassString)
		if storageClassString != noStorageClass &&
			storageClassString != s3.StorageClassStandard &&
			storageClassString != s3.StorageClassReducedRedundancy &&
			storageClassString != s3.StorageClassStandardIa &&
			storageClassString != s3.StorageClassOnezoneIa &&
			storageClassString != s3.StorageClassIntelligentTiering &&
			storageClassString != s3.StorageClassOutposts &&
			storageClassString != s3.StorageClassGlacierIr {
			return nil, fmt.Errorf(
				"the storageclass parameter must be one of %v, %v invalid",
				s3StorageClasses,
				storageClassParam,
			)
		}
		storageClass = storageClassString
	}

	userAgent := parameters["useragent"]
	if userAgent == nil {
		userAgent = ""
	}

	objectACL := s3.ObjectCannedACLPrivate
	objectACLParam := parameters["objectacl"]
	if objectACLParam != nil {
		objectACLString, ok := objectACLParam.(string)
		if !ok {
			return nil, fmt.Errorf("invalid value for objectacl parameter: %v", objectACLParam)
		}

		if _, ok = validObjectACLs[objectACLString]; !ok {
			return nil, fmt.Errorf("invalid value for objectacl parameter: %v", objectACLParam)
		}
		objectACL = objectACLString
	}

	useDualStackBool := false
	useDualStack := parameters["usedualstack"]
	switch useDualStack := useDualStack.(type) {
	case string:
		b, err := strconv.ParseBool(useDualStack)
		if err != nil {
			return nil, fmt.Errorf("the useDualStack parameter should be a boolean")
		}
		useDualStackBool = b
	case bool:
		useDualStackBool = useDualStack
	case nil:
		// do nothing
	default:
		return nil, fmt.Errorf("the useDualStack parameter should be a boolean")
	}

	mutlipartCombineSmallPart := true
	combine := parameters["multipartcombinesmallpart"]
	switch combine := combine.(type) {
	case string:
		b, err := strconv.ParseBool(combine)
		if err != nil {
			return nil, fmt.Errorf("the multipartcombinesmallpart parameter should be a boolean")
		}
		mutlipartCombineSmallPart = b
	case bool:
		mutlipartCombineSmallPart = combine
	case nil:
		// do nothing
	default:
		return nil, fmt.Errorf("the multipartcombinesmallpart parameter should be a boolean")
	}

	sessionToken := parameters["sessiontoken"]

	accelerateBool := false
	accelerate := parameters["accelerate"]
	switch accelerate := accelerate.(type) {
	case string:
		b, err := strconv.ParseBool(accelerate)
		if err != nil {
			return nil, fmt.Errorf("the accelerate parameter should be a boolean")
		}
		accelerateBool = b
	case bool:
		accelerateBool = accelerate
	case nil:
		// do nothing
	default:
		return nil, fmt.Errorf("the accelerate parameter should be a boolean")
	}
	artyMeta := fmt.Sprintf("%s", parameters["metadatapath"])
	if parameters["rootdirectory"] == "" {
		if strings.HasPrefix(artyMeta, "../") {
			return nil, fmt.Errorf("metadata path cant have relative path if rootdirectory is not set")
		}
	}

	params := DriverParameters{
		fmt.Sprint(accessKey),
		fmt.Sprint(secretKey),
		fmt.Sprint(bucket),
		region,
		fmt.Sprint(regionEndpoint),
		forcePathStyleBool,
		encryptBool,
		fmt.Sprint(keyID),
		secureBool,
		skipVerifyBool,
		v4Bool,
		chunkSize,
		multipartCopyChunkSize,
		multipartCopyMaxConcurrency,
		multipartCopyThresholdSize,
		mutlipartCombineSmallPart,
		fmt.Sprint(rootDirectory),
		storageClass,
		fmt.Sprint(userAgent),
		objectACL,
		fmt.Sprint(sessionToken),
		useDualStackBool,
		accelerateBool,
		fmt.Sprint(artyMeta),
	}

	return New(params)
}

// getParameterAsInt64 converts parameters[name] to an int64 value (using
// defaultt if nil), verifies it is no smaller than min, and returns it.
func getParameterAsInt64(parameters map[string]interface{}, name string, defaultt int64, min int64, max int64) (int64, error) {
	rv := defaultt
	param := parameters[name]
	switch v := param.(type) {
	case string:
		vv, err := strconv.ParseInt(v, 0, 64)
		if err != nil {
			return 0, fmt.Errorf("%s parameter must be an integer, %v invalid", name, param)
		}
		rv = vv
	case int64:
		rv = v
	case int, uint, int32, uint32, uint64:
		rv = reflect.ValueOf(v).Convert(reflect.TypeOf(rv)).Int()
	case nil:
		// do nothing
	default:
		return 0, fmt.Errorf("invalid value for %s: %#v", name, param)
	}

	if rv < min || rv > max {
		return 0, fmt.Errorf("the %s %#v parameter should be a number between %d and %d (inclusive)", name, rv, min, max)
	}

	return rv, nil
}

// New constructs a new Driver with the given AWS credentials, region, encryption flag, and
// bucketName
func New(params DriverParameters) (*Driver, error) {
	if !params.V4Auth &&
		(params.RegionEndpoint == "" ||
			strings.Contains(params.RegionEndpoint, "s3.amazonaws.com")) {
		return nil, fmt.Errorf("on Amazon S3 this storage driver can only be used with v4 authentication")
	}

	awsConfig := aws.NewConfig()

	if params.AccessKey != "" && params.SecretKey != "" {
		creds := credentials.NewStaticCredentials(
			params.AccessKey,
			params.SecretKey,
			params.SessionToken,
		)
		awsConfig.WithCredentials(creds)
	}

	if params.RegionEndpoint != "" {
		awsConfig.WithEndpoint(params.RegionEndpoint)
		awsConfig.WithS3ForcePathStyle(params.ForcePathStyle)
	}

	awsConfig.WithS3UseAccelerate(params.Accelerate)
	awsConfig.WithRegion(params.Region)
	awsConfig.WithDisableSSL(!params.Secure)
	if params.UseDualStack {
		awsConfig.UseDualStackEndpoint = endpoints.DualStackEndpointStateEnabled
	}

	if params.SkipVerify {
		httpTransport := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		awsConfig.WithHTTPClient(&http.Client{
			Transport: httpTransport,
		})
	}

	sess, err := session.NewSession(awsConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create new session with aws config: %v", err)
	}

	if params.UserAgent != "" {
		sess.Handlers.Build.PushBack(request.MakeAddToUserAgentFreeFormHandler(params.UserAgent))
	}

	s3obj := s3.New(sess)

	// enable S3 compatible signature v2 signing instead
	if !params.V4Auth {
		setv2Handlers(s3obj)
	}

	// TODO Currently multipart uploads have no timestamps, so this would be unwise
	// if you initiated a new s3driver while another one is running on the same bucket.
	// multis, _, err := bucket.ListMulti("", "")
	// if err != nil {
	// 	return nil, err
	// }

	// for _, multi := range multis {
	// 	err := multi.Abort()
	// 	//TODO appropriate to do this error checking?
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// }

	metaData := make(map[string]string)
	d := &driver{
		S3:                          s3obj,
		Bucket:                      params.Bucket,
		ChunkSize:                   params.ChunkSize,
		Encrypt:                     params.Encrypt,
		KeyID:                       params.KeyID,
		MultipartCopyChunkSize:      params.MultipartCopyChunkSize,
		MultipartCopyMaxConcurrency: params.MultipartCopyMaxConcurrency,
		MultipartCopyThresholdSize:  params.MultipartCopyThresholdSize,
		MultipartCombineSmallPart:   params.MultipartCombineSmallPart,
		RootDirectory:               params.RootDirectory,
		StorageClass:                params.StorageClass,
		ObjectACL:                   params.ObjectACL,
	}
	mPath := params.RootDirectory + params.MetadataPath
	content, err := d.GetContent(context.TODO(), mPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata from path: %s: %s", mPath, err)
	}
	err = json.Unmarshal(content, &metaData)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal metadata: %s", err)
	}
	d.ArtifactoryMetadata = metaData
	return &Driver{
		baseEmbed: baseEmbed{
			Base: base.Base{
				StorageDriver: d,
			},
		},
	}, nil
}

// Implement the storagedriver.StorageDriver interface

func (d *driver) Name() string {
	return driverName
}

func sha1path(s string) string {
	return fmt.Sprintf("%s/%s", s[0:2], s)
}

func (d *driver) ConvertPath(p string) (string, error) {
	if !strings.HasPrefix(p, "/docker/registry") {
		return p, nil
	}
	// artifactory doesn't have docker/registry/v2/repositories path
	sPath := strings.TrimPrefix(p, "/docker/registry/v2")
	return fmt.Sprintf("/%s", sha1path(d.ArtifactoryMetadata[sPath])), nil
}

func handleLink(s string) string {
	return strings.Split(s, "/")[1]
}

// GetContent retrieves the content stored at "path" as a []byte.
func (d *driver) GetContent(ctx context.Context, path string) ([]byte, error) {
	// /docker/registry/v2/repositories/bks-docker-local/cert-manager-controller/_manifests/tags/v0.12.0-venafi/current/link
	nPath, err := d.ConvertPath(path)
	// /bks-docker-local/cert-manager-controller/_manifests/tags/v0.12.0-venafi/current/link
	if strings.HasSuffix(path, "link") {
		// return digest for manifest "sha256:xxxxxx"
		digest := fmt.Sprintf("sha256:%s", strings.Split(nPath, "/")[2])
		return []byte(digest), nil
	}
	if err != nil {
		return nil, err
	}
	reader, err := d.Reader(ctx, nPath, 0)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(reader)
}

// PutContent stores the []byte content at a location designated by "path".
func (d *driver) PutContent(ctx context.Context, path string, contents []byte) error {
	return errors.New("not implemented")
}

// Reader retrieves an io.ReadCloser for the content stored at "path" with a
// given byte offset.
func (d *driver) Reader(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	path, err := d.ConvertPath(path)
	if err != nil {
		return nil, err
	}
	resp, err := d.S3.GetObjectWithContext(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.Bucket),
		Key:    aws.String(d.s3Path(path)),
		Range:  aws.String("bytes=" + strconv.FormatInt(offset, 10) + "-"),
	})
	if err != nil {
		if s3Err, ok := err.(awserr.Error); ok && s3Err.Code() == "InvalidRange" {
			return io.NopCloser(bytes.NewReader(nil)), nil
		}

		return nil, parseError(path, err)
	}
	return resp.Body, nil
}

// Writer returns a FileWriter which will store the content written to it
// at the location designated by "path" after the call to Commit.
func (d *driver) Writer(ctx context.Context, path string, appendParam bool) (storagedriver.FileWriter, error) {
	return nil, errors.New("not implemented")
}

// Stat retrieves the FileInfo for the given path, including the current size
// in bytes and the creation time.
func (d *driver) Stat(ctx context.Context, path string) (storagedriver.FileInfo, error) {
	path, err := d.ConvertPath(path)
	if err != nil {
		return nil, err
	}
	resp, err := d.S3.ListObjectsV2WithContext(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(d.Bucket),
		Prefix:  aws.String(d.s3Path(path)),
		MaxKeys: aws.Int64(1),
	})
	if err != nil {
		return nil, err
	}

	fi := storagedriver.FileInfoFields{
		Path: path,
	}

	if len(resp.Contents) == 1 {
		if *resp.Contents[0].Key != d.s3Path(path) {
			fi.IsDir = true
		} else {
			fi.IsDir = false
			fi.Size = *resp.Contents[0].Size
			fi.ModTime = *resp.Contents[0].LastModified
		}
	} else if len(resp.CommonPrefixes) == 1 {
		fi.IsDir = true
	} else {
		return nil, storagedriver.PathNotFoundError{Path: path}
	}

	return storagedriver.FileInfoInternal{FileInfoFields: fi}, nil
}

// List returns a list of the objects that are direct descendants of the given path.
func (d *driver) List(ctx context.Context, opath string) ([]string, error) {
	path := opath
	if path != "/" && path[len(path)-1] != '/' {
		path = path + "/"
	}

	// This is to cover for the cases when the rootDirectory of the driver is either "" or "/".
	// In those cases, there is no root prefix to replace and we must actually add a "/" to all
	// results in order to keep them as valid paths as recognized by storagedriver.PathRegexp
	prefix := ""
	if d.s3Path("") == "" {
		prefix = "/"
	}

	resp, err := d.S3.ListObjectsV2WithContext(ctx, &s3.ListObjectsV2Input{
		Bucket:    aws.String(d.Bucket),
		Prefix:    aws.String(d.s3Path(path)),
		Delimiter: aws.String("/"),
		MaxKeys:   aws.Int64(listMax),
	})
	if err != nil {
		return nil, parseError(opath, err)
	}

	files := []string{}
	directories := []string{}

	for {
		for _, key := range resp.Contents {
			files = append(files, strings.Replace(*key.Key, d.s3Path(""), prefix, 1))
		}

		for _, commonPrefix := range resp.CommonPrefixes {
			commonPrefix := *commonPrefix.Prefix
			directories = append(directories, strings.Replace(commonPrefix[0:len(commonPrefix)-1], d.s3Path(""), prefix, 1))
		}

		if *resp.IsTruncated {
			resp, err = d.S3.ListObjectsV2WithContext(ctx, &s3.ListObjectsV2Input{
				Bucket:            aws.String(d.Bucket),
				Prefix:            aws.String(d.s3Path(path)),
				Delimiter:         aws.String("/"),
				MaxKeys:           aws.Int64(listMax),
				ContinuationToken: resp.NextContinuationToken,
			})
			if err != nil {
				return nil, err
			}
		} else {
			break
		}
	}

	if opath != "/" {
		if len(files) == 0 && len(directories) == 0 {
			// Treat empty response as missing directory, since we don't actually
			// have directories in s3.
			return nil, storagedriver.PathNotFoundError{Path: opath}
		}
	}

	return append(files, directories...), nil
}

// Move moves an object stored at sourcePath to destPath, removing the original
// object.
func (d *driver) Move(ctx context.Context, sourcePath string, destPath string) error {
	return errors.New("not implemented")
}

// Delete recursively deletes all objects stored at "path" and its subpaths.
// We must be careful since S3 does not guarantee read after delete consistency
func (d *driver) Delete(ctx context.Context, path string) error {
	return errors.New("not implemented")
}

// URLFor returns a URL which may be used to retrieve the content stored at the given path.
// May return an UnsupportedMethodErr in certain StorageDriver implementations.
func (d *driver) URLFor(ctx context.Context, path string, options map[string]interface{}) (string, error) {
	path, err := d.ConvertPath(path)
	if err != nil {
		return "", err
	}
	methodString := http.MethodGet
	method, ok := options["method"]
	if ok {
		methodString, ok = method.(string)
		if !ok || (methodString != http.MethodGet && methodString != http.MethodHead) {
			return "", storagedriver.ErrUnsupportedMethod{}
		}
	}

	expiresIn := 20 * time.Minute
	expires, ok := options["expiry"]
	if ok {
		et, ok := expires.(time.Time)
		if ok {
			expiresIn = time.Until(et)
		}
	}

	var req *request.Request

	switch methodString {
	case http.MethodGet:
		req, _ = d.S3.GetObjectRequest(&s3.GetObjectInput{
			Bucket: aws.String(d.Bucket),
			Key:    aws.String(d.s3Path(path)),
		})
	case http.MethodHead:
		req, _ = d.S3.HeadObjectRequest(&s3.HeadObjectInput{
			Bucket: aws.String(d.Bucket),
			Key:    aws.String(d.s3Path(path)),
		})
	default:
		panic("unreachable")
	}

	return req.Presign(expiresIn)
}

// Walk traverses a filesystem defined within driver, starting
// from the given path, calling f on each file
func (d *driver) Walk(ctx context.Context, from string, f storagedriver.WalkFn, options ...func(*storagedriver.WalkOptions)) error {
	walkOptions := &storagedriver.WalkOptions{}
	for _, o := range options {
		o(walkOptions)
	}

	var objectCount int64
	if err := d.doWalk(ctx, &objectCount, from, walkOptions.StartAfterHint, f); err != nil {
		return err
	}

	return nil
}

func (d *driver) doWalk(parentCtx context.Context, objectCount *int64, from string, startAfter string, f storagedriver.WalkFn) error {
	var (
		retError error
		// the most recent directory walked for de-duping
		prevDir string
		// the most recent skip directory to avoid walking over undesirable files
		prevSkipDir string
	)
	prevDir = from

	path := from
	if !strings.HasSuffix(path, "/") {
		path = path + "/"
	}

	prefix := ""
	if d.s3Path("") == "" {
		prefix = "/"
	}

	listObjectsInput := &s3.ListObjectsV2Input{
		Bucket:     aws.String(d.Bucket),
		Prefix:     aws.String(d.s3Path(path)),
		MaxKeys:    aws.Int64(listMax),
		StartAfter: aws.String(d.s3Path(startAfter)),
	}

	ctx, done := dcontext.WithTrace(parentCtx)
	defer done("s3aws.ListObjectsV2PagesWithContext(%s)", listObjectsInput)

	// When the "delimiter" argument is omitted, the S3 list API will list all objects in the bucket
	// recursively, omitting directory paths. Objects are listed in sorted, depth-first order so we
	// can infer all the directories by comparing each object path to the last one we saw.
	// See: https://docs.aws.amazon.com/AmazonS3/latest/userguide/ListingKeysUsingAPIs.html

	// With files returned in sorted depth-first order, directories are inferred in the same order.
	// ErrSkipDir is handled by explicitly skipping over any files under the skipped directory. This may be sub-optimal
	// for extreme edge cases but for the general use case in a registry, this is orders of magnitude
	// faster than a more explicit recursive implementation.
	listObjectErr := d.S3.ListObjectsV2PagesWithContext(ctx, listObjectsInput, func(objects *s3.ListObjectsV2Output, lastPage bool) bool {
		walkInfos := make([]storagedriver.FileInfoInternal, 0, len(objects.Contents))

		for _, file := range objects.Contents {
			filePath := strings.Replace(*file.Key, d.s3Path(""), prefix, 1)

			// get a list of all inferred directories between the previous directory and this file
			dirs := directoryDiff(prevDir, filePath)
			if len(dirs) > 0 {
				for _, dir := range dirs {
					walkInfos = append(walkInfos, storagedriver.FileInfoInternal{
						FileInfoFields: storagedriver.FileInfoFields{
							IsDir: true,
							Path:  dir,
						},
					})
					prevDir = dir
				}
			}

			walkInfos = append(walkInfos, storagedriver.FileInfoInternal{
				FileInfoFields: storagedriver.FileInfoFields{
					IsDir:   false,
					Size:    *file.Size,
					ModTime: *file.LastModified,
					Path:    filePath,
				},
			})
		}

		for _, walkInfo := range walkInfos {
			// skip any results under the last skip directory
			if prevSkipDir != "" && strings.HasPrefix(walkInfo.Path(), prevSkipDir) {
				continue
			}

			err := f(walkInfo)
			*objectCount++

			if err != nil {
				if err == storagedriver.ErrSkipDir {
					prevSkipDir = walkInfo.Path()
					continue
				}
				if err == storagedriver.ErrFilledBuffer {
					return false
				}
				retError = err
				return false
			}
		}
		return true
	})

	if retError != nil {
		return retError
	}

	if listObjectErr != nil {
		return listObjectErr
	}

	return nil
}

// directoryDiff finds all directories that are not in common between
// the previous and current paths in sorted order.
//
// # Examples
//
//	directoryDiff("/path/to/folder", "/path/to/folder/folder/file")
//	// => [ "/path/to/folder/folder" ]
//
//	directoryDiff("/path/to/folder/folder1", "/path/to/folder/folder2/file")
//	// => [ "/path/to/folder/folder2" ]
//
//	directoryDiff("/path/to/folder/folder1/file", "/path/to/folder/folder2/file")
//	// => [ "/path/to/folder/folder2" ]
//
//	directoryDiff("/path/to/folder/folder1/file", "/path/to/folder/folder2/folder1/file")
//	// => [ "/path/to/folder/folder2", "/path/to/folder/folder2/folder1" ]
//
//	directoryDiff("/", "/path/to/folder/folder/file")
//	// => [ "/path", "/path/to", "/path/to/folder", "/path/to/folder/folder" ]
func directoryDiff(prev, current string) []string {
	var paths []string

	if prev == "" || current == "" {
		return paths
	}

	parent := current
	for {
		parent = filepath.Dir(parent)
		if parent == "/" || parent == prev || strings.HasPrefix(prev+"/", parent+"/") {
			break
		}
		paths = append(paths, parent)
	}
	reverse(paths)
	return paths
}

func reverse(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

func (d *driver) s3Path(path string) string {
	return strings.TrimLeft(strings.TrimRight(d.RootDirectory, "/")+path, "/")
}

// S3BucketKey returns the s3 bucket key for the given storage driver path.
func (d *Driver) S3BucketKey(path string) string {
	return d.StorageDriver.(*driver).s3Path(path)
}

func parseError(path string, err error) error {
	if s3Err, ok := err.(awserr.Error); ok && s3Err.Code() == "NoSuchKey" {
		return storagedriver.PathNotFoundError{Path: path}
	}

	return err
}
