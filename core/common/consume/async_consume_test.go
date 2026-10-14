package consume_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/labring/aiproxy/core/common/consume"
	"github.com/labring/aiproxy/core/model"
	"github.com/labring/aiproxy/core/relay/meta"
	"github.com/labring/aiproxy/core/relay/mode"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type blockingConsumer struct {
	entered chan struct{}
	release chan struct{}
}

func (c *blockingConsumer) PostGroupConsume(context.Context, string, float64) (float64, error) {
	close(c.entered)
	<-c.release
	return 0, nil
}

type panickingConsumer struct{}

func (panickingConsumer) PostGroupConsume(context.Context, string, float64) (float64, error) {
	panic("balance backend exploded")
}

func asyncConsumeFixture(t *testing.T, requestID string) (*gorm.DB, *meta.Meta, model.Usage, model.Price) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}))

	oldLogDB := model.LogDB
	model.LogDB = db
	t.Cleanup(func() {
		model.LogDB = oldLogDB
	})

	requestMeta := &meta.Meta{
		RequestID:   requestID,
		RequestAt:   time.Now(),
		Group:       model.GroupCache{ID: "group"},
		Token:       model.TokenCache{ID: 1, Name: "token"},
		Channel:     meta.ChannelMeta{ID: 2},
		OriginModel: "chat-model",
		Mode:        mode.ChatCompletions,
	}
	usage := model.Usage{InputTokens: 10, OutputTokens: 10, TotalTokens: 20}
	price := model.Price{InputPrice: 1, InputPriceUnit: 1, OutputPrice: 1, OutputPriceUnit: 1}

	return db, requestMeta, usage, price
}

// Shutdown calls consume.Wait() so in-flight debits and log writes finish
// before the process exits. Wait used to return as soon as the goroutine was
// started, so a deploy could drop them.
func TestAsyncConsumeWaitCoversTheDebitAndLogWrite(t *testing.T) {
	db, requestMeta, usage, price := asyncConsumeFixture(t, "async_wait")
	consumer := &blockingConsumer{entered: make(chan struct{}), release: make(chan struct{})}

	consume.AsyncConsume(
		consumer, http.StatusOK, time.Now(), requestMeta, usage, model.UsageContext{}, price,
		"", "127.0.0.1", 0, nil, true, nil, "", model.AsyncUsageStatusNone,
	)

	select {
	case <-consumer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("consume never reached the balance backend")
	}

	waited := make(chan struct{})
	go func() {
		consume.Wait()
		close(waited)
	}()

	select {
	case <-waited:
		t.Fatal("Wait returned while the debit was still in flight")
	case <-time.After(200 * time.Millisecond):
	}

	close(consumer.release)

	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the debit finished")
	}

	var count int64
	require.NoError(t, db.Model(&model.Log{}).Where("request_id = ?", requestMeta.RequestID).Count(&count).Error)
	require.Equal(t, int64(1), count, "the log row is written before Wait returns")
}

// A panic while consuming is logged and recovered in the consuming goroutine;
// it used to escape the deferred recover and crash the whole gateway.
func TestAsyncConsumeRecoversPanicInTheConsumingGoroutine(t *testing.T) {
	_, requestMeta, usage, price := asyncConsumeFixture(t, "async_panic")

	consume.AsyncConsume(
		panickingConsumer{}, http.StatusOK, time.Now(), requestMeta, usage, model.UsageContext{}, price,
		"", "127.0.0.1", 0, nil, true, nil, "", model.AsyncUsageStatusNone,
	)

	done := make(chan struct{})
	go func() {
		consume.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after a panicking consume")
	}
}
