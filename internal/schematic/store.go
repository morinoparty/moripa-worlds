package schematic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"golang.org/x/sync/singleflight"
)

// S3API は Store が使う S3 の操作。*s3.Client が満たす。
type S3API interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, opts ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// S3Options は NewS3Client の設定。
type S3Options struct {
	Endpoint        string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
}

// NewS3Client は RustFS などの S3 互換ストレージ向けに、
// path-style・固定エンドポイント・必要なときだけチェックサムを付ける設定のクライアントを作る。
func NewS3Client(o S3Options) *s3.Client {
	return s3.New(s3.Options{
		Region:                     o.Region,
		BaseEndpoint:               aws.String(o.Endpoint),
		UsePathStyle:               true,
		Credentials:                aws.NewCredentialsCache(staticCredentials{o.AccessKeyID, o.SecretAccessKey}),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		RetryMaxAttempts:           3,
	})
}

type staticCredentials struct{ id, secret string }

func (c staticCredentials) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: c.id, SecretAccessKey: c.secret, Source: "moripa-worlds-env"}, nil
}

// StoreOptions は Store の設定。
type StoreOptions struct {
	Bucket      string
	Prefix      string // "schematics/" のように末尾 "/" 付き
	PositiveTTL time.Duration
	NegativeTTL time.Duration
	Timeout     time.Duration // 1 回の S3 呼び出しの上限
	Now         func() time.Time
	Logger      *slog.Logger
}

type cacheEntry struct {
	info    *Info
	expires time.Time
}

// Store は schematic の info.json を S3 から読む。
// 見つかった結果は PositiveTTL、見つからなかった結果は NegativeTTL の間キャッシュし、
// 同じ ID への同時の問い合わせは 1 回の GetObject にまとめる。
// S3 自体のエラー (ネットワークなど) はキャッシュしない。
type Store struct {
	client S3API
	opts   StoreOptions
	log    *slog.Logger

	mu    sync.Mutex
	cache map[string]cacheEntry
	sf    singleflight.Group
}

// NewStore は Store を作る。
func NewStore(client S3API, opts StoreOptions) *Store {
	if opts.PositiveTTL == 0 {
		opts.PositiveTTL = 30 * time.Second
	}
	if opts.NegativeTTL == 0 {
		opts.NegativeTTL = 10 * time.Second
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Store{client: client, opts: opts, log: opts.Logger, cache: map[string]cacheEntry{}}
}

func (s *Store) dir(id string) string     { return s.opts.Prefix + id + "/" }
func (s *Store) infoKey(id string) string { return s.dir(id) + InfoFileName }

// Get は id の info.json をキャッシュ経由で返す。
// 存在しないか内容が不正な場合は ErrNotFound を返す。
func (s *Store) Get(ctx context.Context, id string) (*Info, error) {
	now := s.opts.Now()
	s.mu.Lock()
	e, ok := s.cache[id]
	s.mu.Unlock()
	if ok && now.Before(e.expires) {
		if e.info == nil {
			return nil, ErrNotFound
		}
		return e.info, nil
	}

	ch := s.sf.DoChan(id, func() (any, error) {
		// 最初の呼び出し元が切断しても他の待機者に影響しないよう、独立したコンテキストで読む。
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.Timeout)
		defer cancel()
		info, err := s.Fetch(fctx, id)
		switch {
		case err == nil:
			s.put(id, info, s.opts.PositiveTTL)
		case errors.Is(err, ErrNotFound):
			s.put(id, nil, s.opts.NegativeTTL)
		}
		return info, err
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Val.(*Info), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Store) put(id string, info *Info, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// キャッシュが際限なく増えないよう、期限切れを掃除する。
	if len(s.cache) > 4096 {
		now := s.opts.Now()
		for k, v := range s.cache {
			if !now.Before(v.expires) {
				delete(s.cache, k)
			}
		}
	}
	s.cache[id] = cacheEntry{info: info, expires: s.opts.Now().Add(ttl)}
}

// Invalidate は id のキャッシュを捨てる。
func (s *Store) Invalidate(id string) {
	s.mu.Lock()
	delete(s.cache, id)
	s.mu.Unlock()
}

// Fetch はキャッシュを使わずに info.json を読む。
// 存在しない・内容が不正な場合は ErrNotFound (不正な場合は ErrInvalid も) を返す。
func (s *Store) Fetch(ctx context.Context, id string) (*Info, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.opts.Bucket),
		Key:    aws.String(s.infoKey(id)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("schematic: get %s: %w", s.infoKey(id), err)
	}
	defer func() { _ = out.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(out.Body, MaxInfoSize+1))
	if err != nil {
		return nil, fmt.Errorf("schematic: read %s: %w", s.infoKey(id), err)
	}
	if len(b) > MaxInfoSize {
		return nil, fmt.Errorf("%w: %w: info.json larger than %d bytes", ErrNotFound, ErrInvalid, MaxInfoSize)
	}
	info, err := ParseInfo(b, id)
	if err != nil {
		s.log.Warn("ignoring invalid info.json", "id", id, "error", err)
		return nil, fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return info, nil
}

// Ping は S3 に到達できるかを確かめる (/readyz 用)。
func (s *Store) Ping(ctx context.Context) error {
	_, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.opts.Bucket),
		Prefix:  aws.String(s.opts.Prefix),
		MaxKeys: aws.Int32(1),
	})
	return err
}

// ListIDs は prefix 直下のディレクトリ名 (schematic ID の候補) を返す。
// 検証はしないので、呼び出し側で worldid.Valid を確かめること。
func (s *Store) ListIDs(ctx context.Context) ([]string, error) {
	var ids []string
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket:    aws.String(s.opts.Bucket),
		Prefix:    aws.String(s.opts.Prefix),
		Delimiter: aws.String("/"),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("schematic: list %s: %w", s.opts.Prefix, err)
		}
		for _, cp := range page.CommonPrefixes {
			name := strings.TrimSuffix(strings.TrimPrefix(aws.ToString(cp.Prefix), s.opts.Prefix), "/")
			if name != "" {
				ids = append(ids, name)
			}
		}
	}
	return ids, nil
}

// maxDeleteBatch は DeleteObjects 1 回で消せる最大キー数。
const maxDeleteBatch = 1000

// DeleteAll は schematics/<id>/ 以下のオブジェクトをすべて削除し、削除したキーを返す。
// 「info.json があれば本体もある」という前提を崩さないよう、info.json を先に消す。
func (s *Store) DeleteAll(ctx context.Context, id string) ([]string, error) {
	defer s.Invalidate(id)
	var deleted []string
	if err := s.deleteKeys(ctx, []string{s.infoKey(id)}); err != nil {
		return nil, err
	}
	deleted = append(deleted, s.infoKey(id))

	var keys []string
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.opts.Bucket),
		Prefix: aws.String(s.dir(id)),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return deleted, fmt.Errorf("schematic: list %s: %w", s.dir(id), err)
		}
		for _, o := range page.Contents {
			if k := aws.ToString(o.Key); k != s.infoKey(id) {
				keys = append(keys, k)
			}
		}
	}
	for start := 0; start < len(keys); start += maxDeleteBatch {
		batch := keys[start:min(start+maxDeleteBatch, len(keys))]
		if err := s.deleteKeys(ctx, batch); err != nil {
			return deleted, err
		}
		deleted = append(deleted, batch...)
	}
	return deleted, nil
}

func (s *Store) deleteKeys(ctx context.Context, keys []string) error {
	objs := make([]types.ObjectIdentifier, len(keys))
	for i, k := range keys {
		objs[i] = types.ObjectIdentifier{Key: aws.String(k)}
	}
	out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(s.opts.Bucket),
		Delete: &types.Delete{Objects: objs, Quiet: aws.Bool(true)},
	})
	if err != nil {
		return fmt.Errorf("schematic: delete objects: %w", err)
	}
	if len(out.Errors) > 0 {
		var errs []error
		for _, e := range out.Errors {
			if aws.ToString(e.Code) == "NoSuchKey" {
				continue
			}
			errs = append(errs, fmt.Errorf("%s: %s %s", aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message)))
		}
		if len(errs) > 0 {
			return fmt.Errorf("schematic: delete objects: %w", errors.Join(errs...))
		}
	}
	return nil
}

func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}
	return false
}
