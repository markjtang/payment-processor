package main

import (
	"fmt"
	"sync"
	"time"
)

func worker(id int, wg *sync.WaitGroup) {
	defer wg.Done() // Notify when this goroutine is done
	fmt.Printf("Worker %d starting\n", id)
	time.Sleep(1 * time.Second) // Simulate work
	fmt.Printf("Worker %d done\n", id)
}

func main() {
	var wg sync.WaitGroup

	// Start 3 goroutines
	for i := 1; i <= 3; i++ {
		wg.Add(1) // Increment the counter for each goroutine
		go worker(i, &wg)
	}

	// Wait for all goroutines to finish
	wg.Wait()
	fmt.Println("All workers done")
}