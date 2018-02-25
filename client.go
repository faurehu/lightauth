package lightauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
)

type path struct {
	localExpirationTime time.Time
	syncExpirationTime  time.Time
	unclaimedInvoices   int
	token               string
	invoices            map[string]*invoice
	mux                 sync.Mutex
	fee                 int
	timePeriod          string
	mode                string
	maxInvoices         int
}

func (p *path) canRequest() bool {
	if p.mode == "time" {
		p.mux.Lock()
		// There's an issue here. Should we use localExpirationTime or syncExpirationTime?
		// If it's local expiration time then we're not fully secure (there might be conflict and should handle the error)
		// If it's syncExpieration time, there is an infinite loop here (syncExpiratoin time only updates with http response, localexpration time updates with lnd payment updates)
		expirationTime := p.localExpirationTime
		p.mux.Unlock()
		return expirationTime.After(time.Now())
	}

	return p.unclaimedInvoices > 0
}

func (p *path) invoiceSettled() {
	if p.mode == "time" {
		timePeriod := time.Millisecond
		switch p.timePeriod {
		case "millisecond":
			timePeriod = time.Millisecond
		case "second":
			timePeriod = time.Second
		case "minute":
			timePeriod = time.Minute
		default:
			timePeriod = time.Millisecond
		}

		p.mux.Lock()
		p.localExpirationTime = p.localExpirationTime.Add(timePeriod)
		p.mux.Unlock()
	} else {
		p.unclaimedInvoices = p.unclaimedInvoices + 1
	}
}

func confirmInvoiceSettled(preImage string) {
	hasher := sha256.New()
	hasher.Write(preImage)
	paymentHash := hex.EncodeToString(hasher.Sum(nil))

	for _, p := range clientStore {
		if i, invoiceExists := p.invoices[paymentHash]; invoiceExists {
			p.invoiceSettled()

			i.mux.Lock()
			i.settled = true
			i.preImage = preImage
			i.mux.Unlock()
		}
	}
}

// ReadResponse will use the information from the response to synchronise info about the protocol status
func ReadResponse(r *http.Response, u string) (*http.Response, error) {
	// TODO: This executions are for the HTTP Status Code 200
	// TODO: Status code paymentrequired : This is where it would be that the local and sync expiration times mismatch gets caught
	// TODO: Stop if the url is not configured for lightauth on server side

	_url, err := url.Parse(u)
	if err != nil {
		log.Fatal(err)
		return r, err
	}

	u = _url.Host + _url.Path
	store := clientStore[u]

	if store.mode == "time" {
		var err error
		store.syncExpirationTime, err = time.Parse("2006-01-02T15:04:05Z07:00", readHeader(r.Header, "Light-Auth-Expiration-Time"))
		if err != nil {
			log.Fatal(err)
			return r, err
		}
	} else {
		if claimedInvoice, exists := store.invoices[paymentHash]; !exists {
			// The invoice sent back by the server does not exist.
			log.Fatal("Server responded successfully with inexistent invoice")
		} else {
			claimedInvoice.claimed = true
			store.unclaimedInvoices = store.unclaimedInvoices - 1
		}
	}

	responseInvoices := strings.Split(readHeader(r.Header, "Light-Auth-Invoices"), ",")
	fee, err := strconv.Atoi(readHeader(r.Header, "Light-Auth-Fee"))
	if err != nil {
		log.Fatal(err)
		return r, err
	}

	for _, v := range responseInvoices {
		paymentHash := getPaymentHash(v)
		if _, invoiceExists := store.invoices[paymentHash]; !invoiceExists {
			store.invoices[paymentHash] = &invoice{paymentRequest: v, fee: fee}
		}
	}

	return r, nil
}

func getInvoicesFromResponse(h http.Header) (map[string]*invoice, error) {
	fee, err := strconv.Atoi(readHeader(h, "Light-Auth-Fee"))
	if err != nil {
		log.Fatal(err)
		return make(map[string]*invoice), err
	}
	invoiceIDs := strings.Split(readHeader(h, "Light-Auth-Invoices"), ",")
	invoices := make(map[string]*invoice)
	for _, v := range invoiceIDs {
		paymentHash := getPaymentHash(v)
		invoices[paymentHash] = &invoice{paymentRequest: v, fee: fee}
	}

	return invoices, nil
}

// ClearRequest is a function used to prepare a request to an API
func ClearRequest(request *http.Request) (*http.Request, error) {
	url := request.URL.Host + request.URL.Path

	if _, routeExists := clientStore[url]; !routeExists {
		response, err := http.Get(request.URL.Scheme + "://" + url)
		if err != nil {
			log.Fatal(err)
			return request, err
		}

		defer response.Body.Close()

		invoices, err := getInvoicesFromResponse(response.Header)
		if err != nil {
			// This here is an error going through 2 lvls of functoin stack. Could contextualize
			return request, err
		}

		// RFC3339
		expirationTime, err := time.Parse("2006-01-02T15:04:05Z07:00", readHeader(response.Header, "Light-Auth-Expiration-Time"))
		if err != nil {
			log.Fatal(err)
			return request, err
		}

		fee, err := strconv.Atoi(readHeader(response.Header, "Light-Auth-Fee"))
		if err != nil {
			log.Fatal(err)
			return request, err
		}

		maxInvoices, err := strconv.Atoi(readHeader(response.Header, "Light-Auth-Max-Invoices"))
		if err != nil {
			log.Fatal(err)
			return request, err
		}

		clientStore[url] = &path{
			invoices:            invoices,
			token:               readHeader(response.Header, "Light-Auth-Token"),
			syncExpirationTime:  expirationTime,
			localExpirationTime: expirationTime,
			fee:                 fee,
			timePeriod:          readHeader(response.Header, "Light-Auth-Time-Period"),
			mode:                readHeader(response.Header, "Light-Auth-Mode"),
			maxInvoices:         maxInvoices,
		}
	}

	routeStore := clientStore[url]
	request.Header.Set("Light-Auth-Token", routeStore.token)

	if routeStore.mode == "time" {
		if routeStore.syncExpirationTime.Before(time.Now()) {
			// TODO: This needs to configure buffer
			for _, v := range routeStore.invoices {
				v.mux.Lock()
				if !v.settled {
					makePayment(v)
				}
				v.mux.Unlock()
			}
		}
	} else {
		if routeStore.unclaimedInvoices < 1 {
			// TODO: This needs to configure buffer
			for _, v := range routeStore.invoices {
				v.mux.Lock()
				if !v.settled {
					makePayment(v)
				}
				v.mux.Unlock()
			}
		}
	}

	// TODO: Should insert a time check here too, to avoid long ass loops
	for {
		if routeStore.canRequest() {
			break
		}
	}

	if routeStore.mode == "discrete" {
		for _, v := range routeStore.invoices {
			v.mux.Lock()
			if v.settled && !v.claimed {
				request.Header.Set("Light-Auth-Pre-Image", v.preImage)
				request.Header.Set("Light-Auth-Invoice", v.paymentRequest)
			}
			v.mux.Unlock()
		}
	}

	return request, nil
}

func getPaymentHash(i string) string {
	ctxb := context.Background()
	PayReqResponse, err := lightningClient.DecodePayReq(ctxb, &lnrpc.PayReqString{PayReq: i})
	if err != nil {
		log.Fatalf("error %v", err)
		return ""
	}

	return PayReqResponse.PaymentHash
}

func makePayment(i *invoice) {
	request := &lnrpc.SendRequest{
		PaymentRequest: i.paymentRequest,
		Amt:            int64(i.fee),
	}

	if err := lightningClientStream.Send(request); err != nil {
		log.Fatalf("Failed to send a payment request: %v", err)
	}
}
