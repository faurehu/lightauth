package lightauth

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
)

const (
	iNVALIDTOKEN       = "Invalid token"
	rOUTENOTCONFIGURED = "Route is not configured"
	tIMEEXPIRED        = "Your authorized time has expired, pay up some balances to buy more time"
	iNVALIDCREDENTIALS = "Invalid credentials"
	mISSINGINVOICE     = "Missing invoice ID"
	mISSINGPREIMAGE    = "Missing pre_image"
)

type invoice struct {
	paymentRequest string
	fee            int
	settled        bool
	preImage       string
}

type client struct {
	token    string
	expires  time.Time
	invoices map[string]invoice
	route    route
}

func (c *client) getUnpayedInvoices() ([]invoice, error) {
	unpayedInvoices := []invoice{}
	for _, i := range c.invoices {
		if !i.settled {
			unpayedInvoices = append(unpayedInvoices, i)
		}
	}

	numUnpayed := len(unpayedInvoices)
	if numUnpayed < c.route.MaxInvoices {
		newInvoices, err := c.generateInvoices(c.route.MaxInvoices - numUnpayed)
		if err != nil {
			return []invoice{}, err
		}

		unpayedInvoices = append(unpayedInvoices, newInvoices...)
	}

	return unpayedInvoices, nil
}

func (c *client) generateInvoices(numberOfInvoices int) ([]invoice, error) {
	ctxb := context.Background()
	invoices := []invoice{}

	for i := 0; i < numberOfInvoices; i++ {
		addInvoiceResponse, err := lightningClient.AddInvoice(ctxb, &lnrpc.Invoice{Value: int64(c.route.Fee)})
		if err != nil {
			log.Fatalf("error %v", err)
			return invoices, err
		}

		invoiceID := addInvoiceResponse.PaymentRequest
		i := invoice{paymentRequest: invoiceID, settled: false}
		invoices = append(invoices, i)
		c.invoices[invoiceID] = i
	}

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

func respondWithInvoices(c *client, message string, w http.ResponseWriter) {
	unpayedInvoices, err := c.getUnpayedInvoices()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	unpayedInvoicesRequests := []string{}
	for _, v := range unpayedInvoices {
		unpayedInvoicesRequests = append(unpayedInvoicesRequests, v.paymentRequest)
	}

	invoicesResponseObj := invoicesResponse{
		Token:    c.token,
		Invoices: unpayedInvoicesRequests,
		Message:  message,
	}

	if c.route.Mode == "time" {
		invoicesResponseObj.ExpirationTime = c.expires.String()
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

func discreteTypeValidator(c *client, w http.ResponseWriter, r *http.Request, handler func(http.ResponseWriter, *http.Request)) {
	invoiceID := r.URL.Query().Get("invoice")
	if invoiceID == "" {
		respondWithInvoices(c, mISSINGINVOICE, w)
		return
	}

	preImage := r.URL.Query().Get("pre_image")
	if preImage == "" {
		http.Error(w, mISSINGPREIMAGE, http.StatusBadRequest)
		return
	}

	i, invoiceExists := c.invoices[invoiceID]
	if !invoiceExists {
		http.Error(w, iNVALIDCREDENTIALS, http.StatusBadRequest)
		return
	}

	if preImage != i.preImage {
		http.Error(w, iNVALIDCREDENTIALS, http.StatusBadRequest)
		return
	}

	handler(w, r)
}

func timeTypeValidator(c *client, w http.ResponseWriter, r *http.Request, handler func(http.ResponseWriter, *http.Request)) {
	expired := c.expires.Before(time.Now())
	if expired {
		respondWithInvoices(c, tIMEEXPIRED, w)
		return
	}

	handler(w, r)
}

// ServerMiddleware is a middleware that checks if the request is valid according to the fees declared for the
// route.
func ServerMiddleware(handler func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		routeName := r.Method + r.URL.Path
		rt, routeExists := store[routeName]
		if !routeExists {
			http.Error(w, rOUTENOTCONFIGURED, http.StatusInternalServerError)
			return
		}

		token := r.URL.Query().Get("token")
		if token == "" {
			for {
				if _, tokenExists := rt.clients[token]; !tokenExists {
					token = randSeq(16)
					rt.clients[token] = &client{token: token, invoices: map[string]invoice{}, expires: time.Now(), route: *rt}
					break
				}
			}
		}

		_, tokenExists := rt.clients[token]
		if !tokenExists {
			http.Error(w, iNVALIDTOKEN, http.StatusBadRequest)
			return
		}

		c := rt.clients[token]
		if rt.Mode == "time" {
			timeTypeValidator(c, w, r, handler)
		} else if rt.Mode == "discrete" {
			discreteTypeValidator(c, w, r, handler)
		}
	}
}
