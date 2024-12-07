// main.go
package main

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    "net/http"
    "os"
    "time"

    "github.com/nats-io/nats.go"
    "github.com/hashicorp/vault/api"
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promauto"
    "github.com/prometheus/client_golang/prometheus/promhttp"
    "payment-processor/pkg/types"
)

// Constants for configuration
const (
    defaultProcessingTimeout = 5 * time.Second
    maxRetries = 3
)

// Metrics for monitoring
var (
    processedTransactions = promauto.NewCounter(prometheus.CounterOpts{
        Name: "payment_processed_transactions_total",
        Help: "The total number of successfully processed transactions",
    })
    failedTransactions = promauto.NewCounter(prometheus.CounterOpts{
        Name: "payment_failed_transactions_total",
        Help: "The total number of failed transactions",
    })
    processingDuration = promauto.NewHistogram(prometheus.HistogramOpts{
        Name: "payment_processing_duration_seconds",
        Help: "Time spent processing payments",
        Buckets: prometheus.LinearBuckets(0.1, 0.1, 10),
    })
)

type PaymentProcessor struct {
    natsConn    *nats.Conn
    vaultClient *api.Client
}

func validateTransaction(tx types.Transaction) error {
    // Validate amount
    if tx.Amount <= 0 {
        return fmt.Errorf("invalid amount: %v", tx.Amount)
    }

    // Validate currency
    validCurrencies := map[string]bool{
        "USD": true,
        "EUR": true,
        "GBP": true,
        "JPY": true,
    }
    if !validCurrencies[tx.Currency] {
        return fmt.Errorf("invalid currency: %s", tx.Currency)
    }

    return nil
}

func NewPaymentProcessor(ctx context.Context) (*PaymentProcessor, error) {
    if err := ctx.Err(); err != nil {
        return nil, fmt.Errorf("context error during initialization: %v", err)
    }

    nc, err := nats.Connect(os.Getenv("NATS_URL"))
    if err != nil {
        return nil, fmt.Errorf("failed to connect to NATS: %v", err)
    }

    config := api.DefaultConfig()
    config.Address = os.Getenv("VAULT_ADDR")

    vaultClient, err := api.NewClient(config)
    if err != nil {
        return nil, fmt.Errorf("failed to create Vault client: %v", err)
    }

    return &PaymentProcessor{
        natsConn:    nc,
        vaultClient: vaultClient,
    }, nil
}

func (p *PaymentProcessor) ProcessPayment(ctx context.Context, tx types.Transaction) error {
    start := time.Now()
    defer func() {
        processingDuration.Observe(time.Since(start).Seconds())
    }()

    // Validate transaction
    if err := validateTransaction(tx); err != nil {
        // Publish to failed transactions topic
        failedTx := tx
        failedTx.Status = "failed"
        txBytes, _ := json.Marshal(failedTx)
        p.natsConn.Publish("payments.failed", txBytes)

        // Increment failed transactions counter
        failedTransactions.Inc()

        return fmt.Errorf("validation failed: %v", err)
    }

    // Process valid transaction
    select {
    case <-time.After(100 * time.Millisecond):
        // Processing completed normally
    case <-ctx.Done():
        failedTransactions.Inc()
        return fmt.Errorf("processing cancelled: %v", ctx.Err())
    }

    tx.Status = "completed"
    tx.Timestamp = time.Now()

    txBytes, err := json.Marshal(tx)
    if err != nil {
        failedTransactions.Inc()
        return fmt.Errorf("failed to marshal transaction: %v", err)
    }

    if err = p.natsConn.Publish("payments.processed", txBytes); err != nil {
        failedTransactions.Inc()
        return fmt.Errorf("failed to publish event: %v", err)
    }

    // Increment successful transactions counter
    processedTransactions.Inc()
    return nil
}

func main() {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    processor, err := NewPaymentProcessor(ctx)
    if err != nil {
        log.Fatalf("Failed to initialize payment processor: %v", err)
    }

    http.Handle("/metrics", promhttp.Handler())
    go func() {
        log.Fatal(http.ListenAndServe(":2112", nil))
    }()

    processor.natsConn.Subscribe("payments.incoming", func(msg *nats.Msg) {
        txCtx, txCancel := context.WithTimeout(ctx, defaultProcessingTimeout)
        defer txCancel()

        var tx types.Transaction
        if err := json.Unmarshal(msg.Data, &tx); err != nil {
            log.Printf("Failed to unmarshal transaction: %v", err)
            failedTransactions.Inc()
            return
        }

        if err := processor.ProcessPayment(txCtx, tx); err != nil {
            log.Printf("Failed to process transaction: %v", err)
            return
        }
    })

    select {
    case <-ctx.Done():
        log.Println("Shutting down payment processor...")
    }
}