package lightauth

import (
	"sync"
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
