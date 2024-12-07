package main

import (
    "encoding/json"
    "fmt"
    "math/rand"
    "os"
    "os/signal"
    "sync"
    "time"

    "github.com/nats-io/nats.go"
    "payment-processor/pkg/types"
)

type TestConfig struct {
    NumTransactions     int
    ConcurrentRequests int
    DelayBetweenReqs   time.Duration
    FailureRate        float64
}

// Enhanced transaction generator with failure scenarios
func generateTransaction(shouldFail bool) types.Transaction {
    amount := 10 + rand.Float64()*990
    currencies := []string{"USD", "EUR", "GBP", "JPY"}
    currency := currencies[rand.Intn(len(currencies))]

    tx := types.Transaction{
        ID:        fmt.Sprintf("test-%d", rand.Int()),
        Currency:  currency,
        Status:    "pending",
        Timestamp: time.Now(),
    }

    if shouldFail {
        // Generate invalid transactions
        switch rand.Intn(3) {
        case 0:
            tx.Amount = -amount // Negative amount
        case 1:
            tx.Amount = 0 // Zero amount
        case 2:
            tx.Currency = "INVALID" // Invalid currency
        }
        fmt.Printf("Generated failing transaction: %+v\n", tx)
    } else {
        tx.Amount = amount
    }

    return tx
}

func runTest(config TestConfig) {
    nc, err := nats.Connect(nats.DefaultURL)
    if err != nil {
        fmt.Printf("Failed to connect to NATS: %v\n", err)
        return
    }
    defer nc.Close()

    results := make(chan string, config.NumTransactions)
    var wg sync.WaitGroup
    var mu sync.Mutex
    successCount := 0
    failureCount := 0

    // Track processed transactions
    processedTxs := make(map[string]bool)

    // Subscribe to processed payments
    nc.Subscribe("payments.processed", func(msg *nats.Msg) {
        var processedTx types.Transaction
        if err := json.Unmarshal(msg.Data, &processedTx); err != nil {
            results <- fmt.Sprintf("FAILED: Unmarshal response: %v", err)
            return
        }

        mu.Lock()
        if _, exists := processedTxs[processedTx.ID]; !exists {
            processedTxs[processedTx.ID] = true
            results <- fmt.Sprintf("SUCCESS: Transaction %s: %.2f %s",
                processedTx.ID, processedTx.Amount, processedTx.Currency)
            successCount++
        }
        mu.Unlock()
        wg.Done()
    })

    // Subscribe to failed payments (new topic)
    nc.Subscribe("payments.failed", func(msg *nats.Msg) {
        var failedTx types.Transaction
        if err := json.Unmarshal(msg.Data, &failedTx); err != nil {
            results <- fmt.Sprintf("FAILED: Unmarshal failed response: %v", err)
            return
        }

        mu.Lock()
        if _, exists := processedTxs[failedTx.ID]; !exists {
            processedTxs[failedTx.ID] = true
            results <- fmt.Sprintf("FAILED: Transaction %s: %.2f %s",
                failedTx.ID, failedTx.Amount, failedTx.Currency)
            failureCount++
        }
        mu.Unlock()
        wg.Done()
    })

    fmt.Printf("Starting test with %d transactions (Failure Rate: %.1f%%)\n",
        config.NumTransactions, config.FailureRate*100)

    sem := make(chan bool, config.ConcurrentRequests)
    interrupt := make(chan os.Signal, 1)
    signal.Notify(interrupt, os.Interrupt)

    startTime := time.Now()

    for i := 0; i < config.NumTransactions; i++ {
        select {
        case <-interrupt:
            fmt.Println("\nTest interrupted by user")
            return
        default:
            sem <- true
            wg.Add(1)

            go func() {
                defer func() { <-sem }()

                shouldFail := rand.Float64() < config.FailureRate
                tx := generateTransaction(shouldFail)

                txBytes, err := json.Marshal(tx)
                if err != nil {
                    mu.Lock()
                    failureCount++
                    mu.Unlock()
                    results <- fmt.Sprintf("FAILED: Marshal error: %v", err)
                    wg.Done()
                    return
                }

                err = nc.Publish("payments.incoming", txBytes)
                if err != nil {
                    mu.Lock()
                    failureCount++
                    mu.Unlock()
                    results <- fmt.Sprintf("FAILED: Publish error: %v", err)
                    wg.Done()
                    return
                }

                if shouldFail {
                    fmt.Printf("Sent failing transaction: %s (Amount: %.2f, Currency: %s)\n",
                        tx.ID, tx.Amount, tx.Currency)
                }
            }()

            time.Sleep(config.DelayBetweenReqs)
        }
    }

    done := make(chan bool)
    go func() {
        wg.Wait()
        done <- true
    }()

    select {
    case <-done:
        duration := time.Since(startTime)
        fmt.Printf("\nTest completed in %v\n", duration)
    case <-interrupt:
        fmt.Println("\nWaiting for in-progress transactions to complete...")
        <-done  // Just wait for the goroutine to finish
    }

    close(results)

    fmt.Println("\nTest Results:")
    for result := range results {
        fmt.Println(result)
    }

    fmt.Printf("\nSummary:\n")
    fmt.Printf("Total Transactions: %d\n", config.NumTransactions)
    fmt.Printf("Successful: %d\n", successCount)
    fmt.Printf("Failed: %d\n", failureCount)
    fmt.Printf("Success Rate: %.2f%%\n",
        float64(successCount)/float64(config.NumTransactions)*100)
}

func main() {
    rand.Seed(time.Now().UnixNano())

    config := TestConfig{
        NumTransactions:     500,   // More transactions
        ConcurrentRequests: 50,    // More concurrent requests
        DelayBetweenReqs:   50 * time.Millisecond,
        FailureRate:        0.4,   // 50% failure rate
    }

    runTest(config)
}