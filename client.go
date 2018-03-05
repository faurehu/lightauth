package lightauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
)

// Path is a hash that stores all of the routes it is authenticating to
type Path struct {
	LocalExpirationTime time.Time
	SyncExpirationTime  time.Time
	Token               string
	Invoices            map[string]*Invoice
	mux                 sync.Mutex
	Fee                 int
	TimePeriod          string
	Mode                string
	MaxInvoices         int
	URL                 string
	ID                  string
}

func (p *Path) getLocalExpirationTime() time.Time {
	p.mux.Lock()
	defer p.mux.Unlock()

	return p.LocalExpirationTime
}

func (p *Path) setLocalExpirationTime(t time.Time) error {
	p.mux.Lock()
	defer p.mux.Unlock()

	p.LocalExpirationTime = t
	return p.save()
}

func (p *Path) setSyncExpirationTime(t time.Time) error {
	p.mux.Lock()
	defer p.mux.Unlock()

	p.SyncExpirationTime = t
	return p.save()
}

func (p *Path) getUnclaimedInvoices() []*Invoice {
	invoices := []*Invoice{}
	for _, v := range p.Invoices {
		if v.Settled && !v.Claimed {
			invoices = append(invoices, v)
		}
	}

	return invoices
}

func (p *Path) save() error {
	if p.ID == "" {
		var err error
		p.ID, err = database.Create(p)
		if err != nil {
			return err
		}
	} else {
		database.Edit(p)
	}

	return nil
}

func (p *Path) canRequest() bool {
	if p.Mode == "time" {
		return p.getLocalExpirationTime().After(time.Now())
	}

	return len(p.getUnclaimedInvoices()) > 0
}

// TODO handle error on invoceSettled calls
func (p *Path) invoiceSettled() error {
	if p.Mode == "time" {
		timePeriod := time.Millisecond
		switch p.TimePeriod {
		case "millisecond":
			timePeriod = time.Millisecond
		case "second":
			timePeriod = time.Second
		case "minute":
			timePeriod = time.Minute
		default:
			timePeriod = time.Millisecond
		}

		t := time.Now()
		localExpirationTime := p.getLocalExpirationTime()

		if localExpirationTime.After(t) {
			diff := localExpirationTime.Sub(t)
			return p.setLocalExpirationTime(t.Add(timePeriod).Add(diff))
		}

		return p.setLocalExpirationTime(t.Add(timePeriod))
	}

	return nil
}

func confirmInvoiceSettled(preImage []byte) {
	hasher := sha256.New()
	hasher.Write(preImage)
	paymentHash := hex.EncodeToString(hasher.Sum(nil))

	for _, p := range clientStore {
		if i, invoiceExists := p.Invoices[paymentHash]; invoiceExists {
			err := p.invoiceSettled()
			if err != nil {
				// TODO: Consider how to handle this scenario
			}

			err = i.settle(preImage)
			if err != nil {
			}

			break
		}
	}
}

// ReadResponse will use the information from the response to synchronise info about the protocol status
func ReadResponse(r *http.Response, u string) (*http.Response, error) {
	// TODO: This executions are for the HTTP Status Code 200
	// TODO: Status code paymentrequired : This is where it would be that the local and sync expiration times mismatch gets caught
	// TODO: Stop if the url is not configured for lightauth on server side

	if r.StatusCode == http.StatusOK {
		_url, err := url.Parse(u)
		if err != nil {
			log.Printf("Lightauth error: The URL is corrupted: %v\n", err)
			return r, err
		}

		u = _url.Host + _url.Path
		store := clientStore[u]

		if store.Mode == "time" {
			var err error
			syncExpirationTime, err := time.Parse("2006-01-02T15:04:05Z07:00", readHeader(r.Header, "Light-Auth-Expiration-Time"))
			if err != nil {
				log.Printf("Lightauth error: Could not read header: %v\n", err)
				return r, err
			}

			err = store.setSyncExpirationTime(syncExpirationTime)
			if err != nil {
				log.Printf("Lightauth error: Could not save path time: %v\n", err)
				return r, err
			}
		} else {
			invoiceID := readHeader(r.Header, "Light-Auth-Invoice")

			var claimedInvoice *Invoice
			for _, v := range store.Invoices {
				if v.PaymentRequest == invoiceID {
					claimedInvoice = v
				}
			}

			if claimedInvoice == nil {
				// TODO: The invoice sent back by the server does not exist.
				log.Printf("Lightauth error: Invoice declared as claimed by server does not exist: %v\n", err)
				return r, err
			}

			err := claimedInvoice.claim()
			if err != nil {
				log.Printf("Lightauth error: Could not save invoice: %v\n", err)
				return r, err
			}
		}

		fee, err := strconv.Atoi(readHeader(r.Header, "Light-Auth-Fee"))
		if err != nil {
			log.Printf("Lightauth error: Could not read header: %v\n", err)
			return r, err
		}

		jsonData := []JSONInvoice{}
		if err := json.Unmarshal([]byte(readHeader(r.Header, "Light-Auth-Invoices")), &jsonData); err != nil {
			log.Printf("Lightauth error: Could not decode header data: %v\n", err)
			return r, err
		}

		for _, v := range jsonData {
			paymentHash, err := getPaymentHash(v.PaymentRequest)
			if err != nil {
				// TODO Server is sending invalid invoice.
				continue
			}

			paymentHashByte, err := hex.DecodeString(paymentHash)
			if err != nil {
				continue
			}

			if _, invoiceExists := store.Invoices[paymentHash]; !invoiceExists {
				i := &Invoice{
					PaymentRequest: v.PaymentRequest,
					Fee:            fee,
					Path:           store,
					PaymentHash:    paymentHashByte,
					ExpirationTime: v.ExpirationTime,
				}
				i.save()

				store.mux.Lock()
				store.Invoices[paymentHash] = i
				store.mux.Unlock()
			}
		}

		return r, nil
	}

	return r, errors.New("Lightauth: The response wasn't successful")
}

func getInvoicesFromResponse(h http.Header) (map[string]*Invoice, error) {
	invoices := make(map[string]*Invoice)
	fee, err := strconv.Atoi(readHeader(h, "Light-Auth-Fee"))
	if err != nil {
		log.Printf("Lightauth error: Failed to read header: %v\n", err)
		return invoices, err
	}

	jsonData := []JSONInvoice{}
	if err := json.Unmarshal([]byte(readHeader(h, "Light-Auth-Invoices")), &jsonData); err != nil {
		log.Printf("Lightauth error: Could not decode header data: %v\n", err)
		return invoices, err
	}

	for _, v := range jsonData {
		paymentHash, err := getPaymentHash(v.PaymentRequest)
		if err != nil {
			// TODO Server is sending invalid invoice.
			continue
		}

		paymentHashByte, err := hex.DecodeString(paymentHash)
		if err != nil {
			continue
		}

		invoices[paymentHash] = &Invoice{
			PaymentRequest: v.PaymentRequest,
			Fee:            fee,
			PaymentHash:    paymentHashByte,
			ExpirationTime: v.ExpirationTime,
		}

		invoices[paymentHash].save()
	}

	return invoices, nil
}

// ClearRequest is a function used to prepare a request to an API
func ClearRequest(request *http.Request) (*http.Request, error) {
	url := request.URL.Host + request.URL.Path

	if _, routeExists := clientStore[url]; !routeExists {
		response, err := http.Get(request.URL.Scheme + "://" + url)
		if err != nil {
			log.Printf("Lightauth error: Couldn't make initial request to route %v\n", err)
			return request, err
		}

		defer response.Body.Close()

		invoices, err := getInvoicesFromResponse(response.Header)
		if err != nil {
			return request, err
		}

		fee, err := strconv.Atoi(readHeader(response.Header, "Light-Auth-Fee"))
		if err != nil {
			log.Printf("Lightauth error: Failed to read header: %v\n", err)
			return request, err
		}

		maxInvoices, err := strconv.Atoi(readHeader(response.Header, "Light-Auth-Max-Invoices"))
		if err != nil {
			log.Printf("Lightauth error: Failed to read header: %v\n", err)
			return request, err
		}

		clientStore[url] = &Path{
			Invoices:    invoices,
			Token:       readHeader(response.Header, "Light-Auth-Token"),
			Fee:         fee,
			MaxInvoices: maxInvoices,
			Mode:        readHeader(response.Header, "Light-Auth-Mode"),
			URL:         url,
		}

		for _, v := range clientStore[url].Invoices {
			v.Path = clientStore[url]
		}

		if clientStore[url].Mode == "time" {
			// RFC3339
			expirationTime, err := time.Parse("2006-01-02T15:04:05Z07:00", readHeader(response.Header, "Light-Auth-Expiration-Time"))
			if err != nil {
				log.Printf("Lightauth error: Failed to read header: %v\n", err)
				return request, err
			}

			clientStore[url].SyncExpirationTime = expirationTime
			clientStore[url].LocalExpirationTime = expirationTime
			clientStore[url].TimePeriod = readHeader(response.Header, "Light-Auth-Time-Period")
		}

		clientStore[url].save()
	}

	routeStore := clientStore[url]
	request.Header.Set("Light-Auth-Token", routeStore.Token)

	if routeStore.Mode == "time" {
		if routeStore.SyncExpirationTime.Before(time.Now()) {
			// TODO: This needs to configure buffer
			for _, v := range routeStore.Invoices {
				if !v.isSettled() {
					err := makePayment(v)
					if err != nil {
						// TODO: We need to make sure at least one payment has been made
						// Otherwise throw error
					}
				}
			}
		}
	} else {
		if len(routeStore.getUnclaimedInvoices()) < 1 {
			// TODO: This needs to configure buffer
			for _, v := range routeStore.Invoices {
				if !v.isSettled() {
					err := makePayment(v)
					if err != nil {
						// TODO: We need to make sure at least one payment has been made
					}
				}
			}
		}
	}

	// TODO: Should insert a time check here too, to avoid long ass loops
	for {
		if routeStore.canRequest() {
			break
		}
	}

	if routeStore.Mode == "discrete" {
		found := false
		for _, v := range routeStore.Invoices {
			if v.isSettled() && !v.isClaimed() && !v.isExpired() {
				preImage := hex.EncodeToString(v.PreImage)
				request.Header.Set("Light-Auth-Pre-Image", preImage)
				request.Header.Set("Light-Auth-Invoice", v.PaymentRequest)
				found = true
				break
			}
		}

		if !found {
			return request, errors.New("Lightauth error: something went wrong")
		}
	}

	return request, nil
}

func getPaymentHash(i string) (string, error) {
	ctxb := context.Background()
	PayReqResponse, err := lightningClient.DecodePayReq(ctxb, &lnrpc.PayReqString{PayReq: i})
	if err != nil {
		log.Printf("Lightauth error: Could not decode payment request: %v\n", err)
		return "", err
	}

	return PayReqResponse.PaymentHash, nil
}

func makePayment(i *Invoice) error {
	request := &lnrpc.SendRequest{
		PaymentRequest: i.PaymentRequest,
		Amt:            int64(i.Fee),
	}

	if err := lightningClientStream.Send(request); err != nil {
		log.Printf("Failed to send a payment request: %v\n", err)
		return err
	}

	return nil
}
