package lightauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	Mux                 sync.Mutex
	Fee                 int
	TimePeriod          string
	Mode                string
	MaxInvoices         int
	URL                 string
	ID                  string
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
		p.Mux.Lock()
		expirationTime := p.LocalExpirationTime
		p.save()
		p.Mux.Unlock()
		return expirationTime.After(time.Now())
	}

	return len(p.getUnclaimedInvoices()) > 0
}

func (p *Path) invoiceSettled() {
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

		p.Mux.Lock()
		p.LocalExpirationTime = p.LocalExpirationTime.Add(timePeriod)
		p.save()
		p.Mux.Unlock()
	}
}

func confirmInvoiceSettled(preImage []byte) {
	hasher := sha256.New()
	hasher.Write(preImage)
	paymentHash := hex.EncodeToString(hasher.Sum(nil))

	for _, p := range clientStore {
		if i, invoiceExists := p.Invoices[paymentHash]; invoiceExists {
			p.invoiceSettled()

			i.Mux.Lock()
			i.Settled = true
			i.PreImage = preImage
			i.save()
			i.Mux.Unlock()
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
			store.Mux.Lock()
			store.SyncExpirationTime, err = time.Parse("2006-01-02T15:04:05Z07:00", readHeader(r.Header, "Light-Auth-Expiration-Time"))
			if err != nil {
				log.Printf("Lightauth error: Could not read header: %v\n", err)
				store.Mux.Unlock()
				return r, err
			}
			store.save()
			store.Mux.Unlock()
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

			claimedInvoice.Mux.Lock()
			claimedInvoice.Claimed = true
			claimedInvoice.save()
			claimedInvoice.Mux.Unlock()
		}

		responseInvoices := strings.Split(readHeader(r.Header, "Light-Auth-Invoices"), ",")
		fee, err := strconv.Atoi(readHeader(r.Header, "Light-Auth-Fee"))
		if err != nil {
			log.Printf("Lightauth error: Could not read header: %v\n", err)
			return r, err
		}

		for _, v := range responseInvoices {
			paymentHash, err := getPaymentHash(v)
			if err != nil {
				// TODO Server is sending invalid invoice.
				continue
			}

			paymentHashByte, err := hex.DecodeString(paymentHash)
			if err != nil {
				continue
			}

			if _, invoiceExists := store.Invoices[paymentHash]; !invoiceExists {
				i := &Invoice{PaymentRequest: v, Fee: fee, Path: store, PaymentHash: paymentHashByte}
				i.Mux.Lock()
				i.save()
				i.Mux.Unlock()

				store.Mux.Lock()
				store.Invoices[paymentHash] = i
				store.save()
				store.Mux.Unlock()
			}
		}

		return r, nil
	}

	return r, errors.New("Lightauth: The response wasn't successful")
}

func getInvoicesFromResponse(h http.Header) (map[string]*Invoice, error) {
	fee, err := strconv.Atoi(readHeader(h, "Light-Auth-Fee"))
	if err != nil {
		log.Printf("Lightauth error: Failed to read header: %v\n", err)
		return make(map[string]*Invoice), err
	}
	invoiceIDs := strings.Split(readHeader(h, "Light-Auth-Invoices"), ",")
	invoices := make(map[string]*Invoice)
	for _, v := range invoiceIDs {
		paymentHash, err := getPaymentHash(v)
		if err != nil {
			// TODO Server is sending invalid invoice.
			continue
		}

		paymentHashByte, err := hex.DecodeString(paymentHash)
		if err != nil {
			continue
		}

		invoices[paymentHash] = &Invoice{PaymentRequest: v, Fee: fee, PaymentHash: paymentHashByte}
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
				v.Mux.Lock()
				if !v.Settled {
					err := makePayment(v)
					if err != nil {
						// TODO: We need to make sure at least one payment has been made
						// Otherwise throw error
					}
				}
				v.Mux.Unlock()
			}
		}
	} else {
		if len(routeStore.getUnclaimedInvoices()) < 1 {
			// TODO: This needs to configure buffer
			for _, v := range routeStore.Invoices {
				v.Mux.Lock()
				if !v.Settled {
					err := makePayment(v)
					if err != nil {
						// TODO: We need to make sure at least one payment has been made
					}
				}
				v.Mux.Unlock()
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
			v.Mux.Lock()
			if v.Settled && !v.Claimed {
				preImage := hex.EncodeToString(v.PreImage)
				request.Header.Set("Light-Auth-Pre-Image", preImage)
				request.Header.Set("Light-Auth-Invoice", v.PaymentRequest)
				v.Mux.Unlock()
				found = true
				break
			}
			v.Mux.Unlock()
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
