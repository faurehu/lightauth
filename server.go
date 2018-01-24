package lightauth

import (
	"context"
	"encoding/json"
	"log"
	"math/rand"
	"net/http"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
)

func randSeq(n int) string {
	chars := []rune("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

	b := make([]rune, n)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

type invoice struct {
	paymentRequest string
	fee            int
	settled        bool
	preImage       string
}

type client struct {
	token    string
	expires  time.Time
	invoices []invoice
}

func (c client) getUnpayedInvoices() []invoice {
	unpayedInvoices := []invoice{}
	for _, i := range c.invoices {
		if !i.settled {
			unpayedInvoices = append(unpayedInvoices, i)
		}
	}
	return unpayedInvoices
}

func (c *client) generateInvoices(numberOfInvoices int) ([]invoice, error) {
	ctxb := context.Background()
	invoices := []invoice{}

	for i := 0; i < numberOfInvoices; i++ {
		addInvoiceResponse, err := lightningClient.AddInvoice(ctxb, &lnrpc.Invoice{Value: 100})
		if err != nil {
			log.Fatalf("error %v", err)
			return invoices, err
		}
		invoices = append(invoices, invoice{paymentRequest: addInvoiceResponse.PaymentRequest, settled: false})
	}

	c.invoices = append(c.invoices, invoices...)
	return invoices, nil
}

type route struct {
	routeInfo
	clients map[string]*client
}

type invoicesResponse struct {
	Message        string
	Invoices       []string
	Token          string
	ExpirationTime string
}

func discreteTypeValidator(_route *route, w http.ResponseWriter, r *http.Request, handler func(http.ResponseWriter, *http.Request)) {
	handler(w, r)
}

func timeTypeValidator(_route *route, w http.ResponseWriter, r *http.Request, handler func(http.ResponseWriter, *http.Request)) {
	token := r.URL.Query().Get("token")
	if token == "" {
		for {
			if _, tokenExists := _route.clients[token]; !tokenExists {
				token = randSeq(16)
				_route.clients[token] = &client{token: token, invoices: []invoice{}, expires: time.Now()}
				break
			}
		}
	}

	_, tokenExists := _route.clients[token]
	if !tokenExists {
		http.Error(w, "Invalid token", http.StatusBadRequest)
		return
	}

	client := _route.clients[token]
	expired := client.expires.Before(time.Now())

	if expired {
		unpayedInvoices := client.getUnpayedInvoices()

		numUnpayed := len(unpayedInvoices)
		if numUnpayed < _route.MaxInvoices {
			newInvoices, err := client.generateInvoices(_route.MaxInvoices - numUnpayed)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			unpayedInvoices = append(unpayedInvoices, newInvoices...)
		}

		unpayedInvoicesRequests := []string{}
		for _, v := range unpayedInvoices {
			unpayedInvoicesRequests = append(unpayedInvoicesRequests, v.paymentRequest)
		}

		invoicesResponseObj := invoicesResponse{
			Message:        "Your authorized time has expired, pay up some balances to buy more time",
			Token:          token,
			Invoices:       unpayedInvoicesRequests,
			ExpirationTime: client.expires.String(),
		}

		js, err := json.Marshal(invoicesResponseObj)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		w.WriteHeader(http.StatusPaymentRequired)
		w.Header().Set("Content-Type", "application/json")
		w.Write(js)
		return
	}

	handler(w, r)
}

// ServerMiddleware is a middleware that checks if the request is valid according to the fees declared for the
// route.
func ServerMiddleware(handler func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		routeName := r.Method + r.URL.Path
		_route, routeExists := store[routeName]
		if !routeExists {
			http.Error(w, "Route is not configured", http.StatusInternalServerError)
			return
		}

		if _route.Mode == "time" {
			timeTypeValidator(_route, w, r, handler)
		} else if _route.Mode == "discrete" {
			discreteTypeValidator(_route, w, r, handler)
		}
	}
}
