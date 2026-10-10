package utils

import (
	"github.com/labring/aiproxy/core/relay/meta"
	"testing"
)

func TestAWSGenerationHasNoHiddenRetries(t *testing.T) {
	m := &meta.Meta{}
	m.Channel.Key = "us-east-1|fake-access|fake-secret"
	client, err := awsClientFromMeta(m)
	if err != nil {
		t.Fatal(err)
	}
	if got := client.Options().Retryer.MaxAttempts(); got != 1 {
		t.Fatalf("SDK retries unsafe submissions: max attempts=%d", got)
	}
}
