package types

import "time"

type Transaction struct {
    ID        string    `json:"id"`
    Amount    float64   `json:"amount"`
    Currency  string    `json:"currency"`
    Status    string    `json:"status"`
    Timestamp time.Time `json:"timestamp"`
}