//go:build integration

package schematic

// 実際の RustFS (S3 互換) に対して Store を確かめる統合テスト。
//
//	docker run -d --name mw-rustfs -p 127.0.0.1:39000:9000 \
//	  -e RUSTFS_ACCESS_KEY=mwtestaccess -e RUSTFS_SECRET_KEY=mwtestsecret123 rustfs/rustfs:1.0.1
//	S3_ENDPOINT=http://127.0.0.1:39000 AWS_ACCESS_KEY_ID=mwtestaccess AWS_SECRET_ACCESS_KEY=mwtestsecret123 \
//	  go test -tags integration -run Integration -v ./internal/schematic/
//
// 本番と同じ権限 (moripa-utils/schematics/* の GetObject・DeleteObject とバケットの ListBucket) の
// キーで確かめるときは、AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY にそのキーを、
// S3_ADMIN_ACCESS_KEY_ID / S3_ADMIN_SECRET_ACCESS_KEY に準備用 (PutObject できる) キーを、
// S3_BUCKET に既存のバケット名を渡す。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestIntegrationRustFS(t *testing.T) {
	endpoint := os.Getenv("S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3_ENDPOINT is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	scoped := NewS3Client(S3Options{
		Endpoint:        endpoint,
		Region:          "us-east-1",
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	})
	client := scoped
	if ak := os.Getenv("S3_ADMIN_ACCESS_KEY_ID"); ak != "" {
		client = NewS3Client(S3Options{Endpoint: endpoint, Region: "us-east-1", AccessKeyID: ak, SecretAccessKey: os.Getenv("S3_ADMIN_SECRET_ACCESS_KEY")})
	}
	bucket := os.Getenv("S3_BUCKET")
	ownBucket := bucket == ""
	if ownBucket {
		bucket = fmt.Sprintf("mw-it-%d", time.Now().UnixNano())
		if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Fatalf("CreateBucket: %v", err)
		}
	}
	put := func(key, body string) {
		t.Helper()
		_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader(body)})
		if err != nil {
			t.Fatalf("PutObject %s: %v", key, err)
		}
	}
	// MoripaUtils と同じ順序: 本体 → info.json
	put("schematics/"+id1+"/schematic.schem", "\x1f\x8bfake-schem")
	put("schematics/"+id1+"/info.json", infoJSON(id1, "2000-01-01T00:00:00Z"))
	put("schematics/"+id2+"/schematic.schem", "\x1f\x8bfake-schem")
	put("schematics/"+id2+"/info.json", infoJSON(id2, ""))
	// 1000 件を超える削除でバッチが分かれることを確かめるための余分なオブジェクト
	for i := range 1005 {
		put(fmt.Sprintf("schematics/%s/extra/%04d", id1, i), "x")
	}

	store := NewStore(scoped, StoreOptions{Bucket: bucket, Prefix: "schematics/", Logger: quiet})

	if err := store.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	info, err := store.Get(ctx, id2)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.FileName != "schematic.schem" || info.SpawnPosition.X != 2.5 || info.DeleteAt != nil {
		t.Fatalf("info = %+v", info)
	}
	if _, err := store.Get(ctx, "0199c8a0-7b3e-7f12-9a4c-00000000dead"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: err = %v", err)
	}

	ids, err := store.ListIDs(ctx)
	if err != nil {
		t.Fatalf("ListIDs: %v", err)
	}
	if strings.Join(ids, ",") != id1+","+id2 {
		t.Fatalf("ListIDs = %v", ids)
	}

	var deletedWorlds []string
	sw := &Sweeper{Store: store, Log: quiet, DeleteWorld: func(_ context.Context, id string) error {
		deletedWorlds = append(deletedWorlds, id)
		return nil
	}}
	swept, err := sw.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if len(swept) != 1 || swept[0] != id1 || len(deletedWorlds) != 1 {
		t.Fatalf("swept = %v, worlds = %v", swept, deletedWorlds)
	}
	out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String("schematics/" + id1 + "/")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Contents) != 0 {
		t.Fatalf("%d objects left under %s", len(out.Contents), id1)
	}
	if _, err := store.Fetch(ctx, id2); err != nil {
		t.Fatalf("id2 must remain: %v", err)
	}
	b, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("schematics/" + id2 + "/schematic.schem")})
	if err != nil {
		t.Fatalf("id2 body: %v", err)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(b.Body)
	_ = b.Body.Close()
	t.Logf("swept %v; %s body %d bytes remain", swept, id2, buf.Len())

	// 後片付け
	_, _ = store.DeleteAll(ctx, id2)
	if !ownBucket {
		return
	}
	if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
		var nf *types.NoSuchBucket
		if !errors.As(err, &nf) {
			t.Logf("DeleteBucket: %v", err)
		}
	}
}
