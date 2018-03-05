package lightauth

import (
	"encoding/json"
	"log"
	"sync"
	"time"
)

// Invoice is a hash that stores all the information of an invoice
type Invoice struct {
	Client         *Client
	PaymentRequest string
	PaymentHash    []byte
	Fee            int
	Settled        bool
	PreImage       []byte
	Claimed        bool
	Path           *Path
	mux            sync.Mutex
	ID             string
	ExpirationTime time.Time
}

// JSONInvoice is a struct to be encoded
type JSONInvoice struct {
	PaymentRequest string    `json:"payment_request"`
	ExpirationTime time.Time `json:"expiration_time"`
}

func getInvoicesJSON(invoices []*Invoice) (string, error) {
	data := []JSONInvoice{}
	for _, v := range invoices {
		data = append(data, JSONInvoice{
			PaymentRequest: v.PaymentRequest,
			ExpirationTime: v.ExpirationTime,
		})
	}
	jsonData, err := json.Marshal(data)
	if err != nil {
		log.Fatalf("Lightauth error: could not encode invoices to JSON %v\n", err)
		return "", err
	}

	return string(jsonData), nil
}

func (i *Invoice) settle(preImage []byte) error {
	i.mux.Lock()
	defer i.mux.Unlock()

	i.Settled = true
	i.PreImage = preImage

	return i.save()
}

func (i *Invoice) isSettled() bool {
	i.mux.Lock()
	defer i.mux.Unlock()

	return i.Settled
}

func (i *Invoice) isClaimed() bool {
	i.mux.Lock()
	defer i.mux.Unlock()

	return i.Claimed
}

func (i *Invoice) isExpired() bool {
	i.mux.Lock()
	defer i.mux.Unlock()

	return i.ExpirationTime.Before(time.Now())
}

func (i *Invoice) claim() error {
	i.mux.Lock()
	defer i.mux.Unlock()

	i.Claimed = true
	return i.save()
}

func (i *Invoice) save() error {
	if i.ID == "" {
		var err error
		i.ID, err = database.Create(i)
		if err != nil {
			return err
		}
	} else {
		database.Edit(i)
	}

	return nil
}
