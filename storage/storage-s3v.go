package storage

import (
	"bytes"
	"context"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/larrabee/ratelimit"
	"io"
	"net/url"
	"path/filepath"
	"time"
)

// S3vStorage configuration.
type S3vStorage struct {
	awsSvc        *s3.S3
	awsSession    *session.Session
	awsBucket     *string
	prefix        string
	keysPerReq    int64
	retryCnt      uint
	retryInterval time.Duration
	ctx           context.Context
	listMarker    *string
	rlBucket      ratelimit.Bucket
}

// NewS3vStorage return new configured S3 storage.
// You should always create new storage with this constructor.
//
// It differs from S3 storage in that it can work with file versions.
func NewS3vStorage(awsAccessKey, awsSecretKey, awsRegion, endpoint, bucketName, prefix string, keysPerReq int64, retryCnt uint, retryInterval time.Duration) *S3vStorage {
	sess := session.Must(session.NewSession())
	sess.Config.S3ForcePathStyle = aws.Bool(true)
	sess.Config.CredentialsChainVerboseErrors = aws.Bool(true)
	sess.Config.Region = aws.String(awsRegion)

	if awsAccessKey != "" && awsSecretKey != "" {
		cred := credentials.NewStaticCredentials(awsAccessKey, awsSecretKey, "")
		sess.Config.WithCredentials(cred)
	} else {
		cred := credentials.NewChainCredentials(
			[]credentials.Provider{
				&credentials.EnvProvider{},
				&credentials.SharedCredentialsProvider{},
				&ec2rolecreds.EC2RoleProvider{
					Client: ec2metadata.New(sess),
				},
			})
		sess.Config.WithCredentials(cred)
	}

	if endpoint != "" {
		sess.Config.Endpoint = aws.String(endpoint)
	}

	storage := S3vStorage{
		awsBucket:     &bucketName,
		awsSession:    sess,
		awsSvc:        s3.New(sess),
		prefix:        prefix,
		keysPerReq:    keysPerReq,
		retryCnt:      retryCnt,
		retryInterval: retryInterval,
		ctx:           context.TODO(),
		rlBucket:      ratelimit.NewFakeBucket(),
	}

	return &storage
}

// WithContext add's context to storage.
func (storage *S3vStorage) WithContext(ctx context.Context) {
	storage.ctx = ctx
}

// WithRateLimit set rate limit (bytes/sec) for storage.
func (storage *S3vStorage) WithRateLimit(limit int) error {
	bucket, err := ratelimit.NewBucketWithRate(float64(limit), int64(limit))
	if err != nil {
		return err
	}
	storage.rlBucket = bucket
	return nil
}

// List S3 bucket and send founded objects versions to chan.
func (storage *S3vStorage) List(output chan<- *Object) error {
	listObjectsFn := func(p *s3.ListObjectVersionsOutput, lastPage bool) bool {
		for _, o := range p.Versions {
			key, _ := url.QueryUnescape(aws.StringValue(o.Key))
			output <- &Object{Key: &key, VersionId: o.VersionId, ETag: strongEtag(o.ETag), Mtime: o.LastModified, IsLatest: o.IsLatest}
		}
		storage.listMarker = p.VersionIdMarker
		return !lastPage // continue paging
	}

	for i := uint(0); ; i++ {
		input := &s3.ListObjectVersionsInput{
			Bucket:          storage.awsBucket,
			Prefix:          aws.String(storage.prefix),
			MaxKeys:         aws.Int64(storage.keysPerReq),
			EncodingType:    aws.String(s3.EncodingTypeUrl),
			VersionIdMarker: storage.listMarker,
		}
		err := storage.awsSvc.ListObjectVersionsPagesWithContext(storage.ctx, input, listObjectsFn)
		if (err != nil) && (i < storage.retryCnt) {
			Log.Debugf("S3 listing failed with error: %s", err)
			time.Sleep(storage.retryInterval)
			continue
		} else if (err != nil) && (i == storage.retryCnt) {
			Log.Debugf("S3 listing failed with error: %s", err)
			return err
		} else {
			Log.Debugf("Listing bucket finished")
			return err
		}
	}
}

// PutObject saves object to S3.
// PutObject ignore VersionId, it always save object as latest version.
func (storage *S3vStorage) PutObject(obj *Object) error {
	objReader := bytes.NewReader(*obj.Content)
	rlReader := ratelimit.NewReadSeeker(objReader, storage.rlBucket)

	input := &s3.PutObjectInput{
		Bucket:             storage.awsBucket,
		Key:                aws.String(filepath.Join(storage.prefix, *obj.Key)),
		Body:               rlReader,
		ContentType:        obj.ContentType,
		ContentDisposition: obj.ContentDisposition,
		ContentEncoding:    obj.ContentEncoding,
		ContentLanguage:    obj.ContentLanguage,
		ACL:                obj.ACL,
		Metadata:           obj.Metadata,
		CacheControl:       obj.CacheControl,
	}

	for i := uint(0); ; i++ {
		_, err := storage.awsSvc.PutObjectWithContext(storage.ctx, input)
		if (err != nil) && (i < storage.retryCnt) {
			Log.Debugf("S3 obj uploading failed with error: %s", err)
			time.Sleep(storage.retryInterval)
			continue
		} else if (err != nil) && (i == storage.retryCnt) {
			return err
		}

		return nil
	}
}

// GetObjectContent read object content and metadata from S3.
func (storage *S3vStorage) GetObjectContent(obj *Object) error {
	input := &s3.GetObjectInput{
		Bucket:    storage.awsBucket,
		Key:       obj.Key,
		VersionId: obj.VersionId,
	}

	for i := uint(0); ; i++ {
		result, err := storage.awsSvc.GetObjectWithContext(storage.ctx, input)
		if (err != nil) && (i < storage.retryCnt) {
			Log.Debugf("S3 obj content downloading request failed with error: %s", err)
			time.Sleep(storage.retryInterval)
			continue
		} else if (err != nil) && (i == storage.retryCnt) {
			return err
		}

		buf := bytes.NewBuffer(make([]byte, 0, aws.Int64Value(result.ContentLength)))
		_, err = io.Copy(ratelimit.NewWriter(buf, storage.rlBucket), result.Body)
		if (err != nil) && (i < storage.retryCnt) {
			Log.Debugf("S3 obj content downloading failed with error: %s", err)
			time.Sleep(storage.retryInterval)
			continue
		} else if (err != nil) && (i == storage.retryCnt) {
			return err
		}

		data := buf.Bytes()
		obj.Content = &data
		obj.ContentType = result.ContentType
		obj.ContentDisposition = result.ContentDisposition
		obj.ContentEncoding = result.ContentEncoding
		obj.ContentLanguage = result.ContentLanguage
		obj.ETag = strongEtag(result.ETag)
		obj.Metadata = result.Metadata
		obj.Mtime = result.LastModified
		obj.CacheControl = result.CacheControl

		return nil
	}
}

// GetObjectMeta update object metadata from S3.
func (storage *S3vStorage) GetObjectMeta(obj *Object) error {
	input := &s3.HeadObjectInput{
		Bucket:    storage.awsBucket,
		Key:       obj.Key,
		VersionId: obj.VersionId,
	}

	for i := uint(0); ; i++ {
		result, err := storage.awsSvc.HeadObjectWithContext(storage.ctx, input)
		if (err != nil) && (i < storage.retryCnt) {
			Log.Debugf("S3 obj meta downloading request failed with error: %s", err)
			time.Sleep(storage.retryInterval)
			continue
		} else if (err != nil) && (i == storage.retryCnt) {
			return err
		}

		obj.ContentType = result.ContentType
		obj.ContentDisposition = result.ContentDisposition
		obj.ContentEncoding = result.ContentEncoding
		obj.ContentLanguage = result.ContentLanguage
		obj.ETag = strongEtag(result.ETag)
		obj.Metadata = result.Metadata
		obj.Mtime = result.LastModified
		obj.CacheControl = result.CacheControl

		return nil
	}
}

// DeleteObject remove object from S3.
func (storage *S3vStorage) DeleteObject(obj *Object) error {
	input := &s3.DeleteObjectInput{
		Bucket:    storage.awsBucket,
		Key:       obj.Key,
		VersionId: obj.VersionId,
	}

	for i := uint(0); ; i++ {
		_, err := storage.awsSvc.DeleteObjectWithContext(storage.ctx, input)
		if (err != nil) && (i < storage.retryCnt) {
			Log.Debugf("S3 obj removing failed with error: %s", err)
			time.Sleep(storage.retryInterval)
			continue
		} else if (err != nil) && (i == storage.retryCnt) {
			return err
		}

		return nil
	}
}

// GetStorageType return storage type.
func (storage *S3vStorage) GetStorageType() Type {
	return TypeS3Versioned
}
