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

type TestResults struct {
	mu           sync.Mutex
	successCount int
	failureCount int
	processedTxs map[string]bool
	results      chan string
}

func NewTestResults(bufferSize int) *TestResults {
	return &TestResults{
		processedTxs: make(map[string]bool),
		results:      make(chan string, bufferSize),
	}
}

func (tr *TestResults) recordTransaction(id string, success bool, msg string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	if _, exists := tr.processedTxs[id]; !exists {
		tr.processedTxs[id] = true
		tr.results <- msg
		if success {
			tr.successCount++
		} else {
			tr.failureCount++
		}
	}
}

type TransactionGenerator struct {
	currencies []string
}

func NewTransactionGenerator() *TransactionGenerator {
	return &TransactionGenerator{
		currencies: []string{"USD", "EUR", "GBP", "JPY"},
	}
}

func (tg *TransactionGenerator) Generate(shouldFail bool) types.Transaction {
	amount := 10 + rand.Float64()*990
	tx := types.Transaction{
		ID:        fmt.Sprintf("test-%d", rand.Int()),
		Currency:  tg.currencies[rand.Intn(len(tg.currencies))],
		Status:    "pending",
		Timestamp: time.Now(),
	}

	if shouldFail {
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

type PaymentTest struct {
	config    TestConfig
	nc        *nats.Conn
	results   *TestResults
	generator *TransactionGenerator
}

func NewPaymentTest(config TestConfig) (*PaymentTest, error) {
	nc, err := nats.Connect(nats.DefaultURL)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to NATS: %v", err)
	}

	return &PaymentTest{
		config:    config,
		nc:        nc,
		results:   NewTestResults(config.NumTransactions),
		generator: NewTransactionGenerator(),
	}, nil
}

func (pt *PaymentTest) setupSubscriptions(wg *sync.WaitGroup) error {
	// Subscribe to processed payments
	_, err := pt.nc.Subscribe("payments.processed", func(msg *nats.Msg) {
		defer wg.Done()
		var processedTx types.Transaction
		if err := json.Unmarshal(msg.Data, &processedTx); err != nil {
			pt.results.recordTransaction("", false, fmt.Sprintf("FAILED: Unmarshal response: %v", err))
			return
		}

		pt.results.recordTransaction(
			processedTx.ID,
			true,
			fmt.Sprintf("SUCCESS: Transaction %s: %.2f %s",
				processedTx.ID, processedTx.Amount, processedTx.Currency),
		)
	})
	if err != nil {
		return err
	}

	// Subscribe to failed payments
	_, err = pt.nc.Subscribe("payments.failed", func(msg *nats.Msg) {
		defer wg.Done()
		var failedTx types.Transaction
		if err := json.Unmarshal(msg.Data, &failedTx); err != nil {
			pt.results.recordTransaction("", false, fmt.Sprintf("FAILED: Unmarshal failed response: %v", err))
			return
		}

		pt.results.recordTransaction(
			failedTx.ID,
			false,
			fmt.Sprintf("FAILED: Transaction %s: %.2f %s",
				failedTx.ID, failedTx.Amount, failedTx.Currency),
		)
	})
	return err
}

func (pt *PaymentTest) publishTransaction(wg *sync.WaitGroup, sem chan bool) {
	defer func() { <-sem }()

	shouldFail := rand.Float64() < pt.config.FailureRate
	tx := pt.generator.Generate(shouldFail)

	txBytes, err := json.Marshal(tx)
	if err != nil {
		pt.results.recordTransaction("", false, fmt.Sprintf("FAILED: Marshal error: %v", err))
		wg.Done()
		return
	}

	if err := pt.nc.Publish("payments.incoming", txBytes); err != nil {
		pt.results.recordTransaction("", false, fmt.Sprintf("FAILED: Publish error: %v", err))
		wg.Done()
		return
	}

	if shouldFail {
		fmt.Printf("Sent failing transaction: %s (Amount: %.2f, Currency: %s)\n",
			tx.ID, tx.Amount, tx.Currency)
	}
}

func (pt *PaymentTest) Run() error {
	defer pt.nc.Close()

	var wg sync.WaitGroup
	if err := pt.setupSubscriptions(&wg); err != nil {
		return fmt.Errorf("failed to setup subscriptions: %v", err)
	}

	fmt.Printf("Starting test with %d transactions (Failure Rate: %.1f%%)\n",
		pt.config.NumTransactions, pt.config.FailureRate*100)

	sem := make(chan bool, pt.config.ConcurrentRequests)
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	startTime := time.Now()

	// Start publishing transactions
	for i := 0; i < pt.config.NumTransactions; i++ {
		select {
		case <-interrupt:
			fmt.Println("\nTest interrupted by user")
			return nil
		default:
			sem <- true
			wg.Add(1)
			go pt.publishTransaction(&wg, sem)
			time.Sleep(pt.config.DelayBetweenReqs)
		}
	}

	// Wait for completion or interruption
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
		<-done
	}

	return pt.printResults()
}

func (pt *PaymentTest) printResults() error {
	close(pt.results.results)

	fmt.Println("\nTest Results:")
	for result := range pt.results.results {
		fmt.Println(result)
	}

	fmt.Printf("\nSummary:\n")
	fmt.Printf("Total Transactions: %d\n", pt.config.NumTransactions)
	fmt.Printf("Successful: %d\n", pt.results.successCount)
	fmt.Printf("Failed: %d\n", pt.results.failureCount)
	fmt.Printf("Success Rate: %.2f%%\n",
		float64(pt.results.successCount)/float64(pt.config.NumTransactions)*100)

	return nil
}

func main() {
	rand.Seed(time.Now().UnixNano())

	config := TestConfig{
		NumTransactions:     500,
		ConcurrentRequests: 50,
		DelayBetweenReqs:   50 * time.Millisecond,
		FailureRate:        0.4,
	}

	test, err := NewPaymentTest(config)
	if err != nil {
		fmt.Printf("Failed to create payment test: %v\n", err)
		return
	}

	if err := test.Run(); err != nil {
		fmt.Printf("Test failed: %v\n", err)
	}
}