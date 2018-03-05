package lightauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/dchest/uniuri"
	"github.com/lightningnetwork/lnd/lnrpc"
)

const (
	iNVALIDTOKEN          = "Lightauth error: Invalid token"
	tIMEEXPIRED           = "Lightauth error: Your authorized time has expired, pay up some balances to buy more time"
	iNVALIDCREDENTIALS    = "Lightauth error: Invalid credentials"
	mISSINGINVOICE        = "Lightauth error: Missing invoice ID"
	mISSINGPREIMAGE       = "Lightauth error: Missing pre_image"
	tRYAGAIN              = "Lightauth error: We can't validate your payment yet, please try again"
	iNVOICEALREADYCLAIMED = "Lightauth error: Invoice has already been claimed"
	sOMETHINGWENTWRONG    = "Lightauth error: Something went wrong"
)

// Route is a hash that stores all the information of a specific endpoint
type Route struct {
	RouteInfo
	Clients map[string]*Client
	ID      string
}

func (r *Route) save() error {
	var err error
	r.ID, err = database.Create(r)
	if err != nil {
		return err
	}

	return nil
}

// Client is a hash that stores all the information of a server's client
type Client struct {
	Token          string
	ExpirationTime time.Time
	Invoices       map[string]*Invoice
	Route          *Route
	ID             string
	mux            sync.Mutex
}

func (c *Client) setExpirationTime(t time.Time) error {
	c.mux.Lock()
	defer c.mux.Unlock()

	c.ExpirationTime = t
	return c.save()
}

func (c *Client) getExpirationTime() time.Time {
	c.mux.Lock()
	defer c.mux.Unlock()

	return c.ExpirationTime
}

func (c *Client) save() error {
	if c.ID == "" {
		var err error
		c.ID, err = database.Create(c)
		if err != nil {
			return err
		}
	} else {
		database.Edit(c)
	}

	return nil
}

func writeConstantHeaders(w http.ResponseWriter, rt RouteInfo) {
	w.Header().Set("Light-Auth-Name", rt.Name)
	w.Header().Set("Light-Auth-Mode", rt.Mode)
	w.Header().Set("Light-Auth-Fee", strconv.Itoa(rt.Fee))
	w.Header().Set("Light-Auth-Max-Invoices", strconv.Itoa(rt.MaxInvoices))

	if rt.Mode == "time" {
		w.Header().Set("Light-Auth-Time-Period", rt.Period)
	}
}

func writeClientHeaders(w http.ResponseWriter, c *Client) error {
	unpayedInvoices, err := c.getUnpayedInvoices()
	if err != nil {
		http.Error(w, "Something went wrong", http.StatusInternalServerError)
		return err
	}

	unpayedInvoicesRequests := []*Invoice{}
	for _, v := range unpayedInvoices {
		unpayedInvoicesRequests = append(unpayedInvoicesRequests, v)
	}

	invoicesJSON, err := getInvoicesJSON(unpayedInvoicesRequests)
	if err != nil {
		return err
	}

	w.Header().Set("Light-Auth-Token", c.Token)
	w.Header().Set("Light-Auth-Invoices", invoicesJSON)

	if c.Route.Mode == "time" {
		// RFC3339
		w.Header().Set("Light-Auth-Expiration-Time", c.ExpirationTime.Format("2006-01-02T15:04:05Z07:00"))
	}

	return err
}

func updateInvoice(paymentRequest string) error {
	for _, r := range serverStore {
		for _, c := range r.Clients {
			if i, invoiceExists := c.Invoices[paymentRequest]; invoiceExists {
				err := i.settle([]byte{})
				if err != nil {
					return err
				}

				if c.Route.Mode == "time" {
					timePeriod := time.Millisecond
					switch c.Route.Period {
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
					expirationTime := c.getExpirationTime()
					if expirationTime.After(t) {
						diff := expirationTime.Sub(t)
						return c.setExpirationTime(t.Add(timePeriod).Add(diff))
					}

					return c.setExpirationTime(t.Add(timePeriod))
				}
			}
		}
	}

	return nil
}

func (c *Client) getUnpayedInvoices() ([]*Invoice, error) {
	unpayedInvoices := []*Invoice{}
	for _, i := range c.Invoices {
		if !i.isSettled() {
			unpayedInvoices = append(unpayedInvoices, i)

		}
	}

	numUnpayed := len(unpayedInvoices)
	if numUnpayed < c.Route.MaxInvoices {
		newInvoices, err := c.generateInvoices(c.Route.MaxInvoices - numUnpayed)
		if err != nil {
			return []*Invoice{}, err
		}

		unpayedInvoices = append(unpayedInvoices, newInvoices...)
	}

	return unpayedInvoices, nil
}

func (c *Client) generateInvoices(numberOfInvoices int) ([]*Invoice, error) {
	ctxb := context.Background()
	invoices := []*Invoice{}

	for i := 0; i < numberOfInvoices; i++ {
		addInvoiceResponse, err := lightningClient.AddInvoice(ctxb, &lnrpc.Invoice{Value: int64(c.Route.Fee)})
		if err != nil {
			log.Printf("Lightauth error: Failed to generate an invoice in the lighting node: %v\n", err)
			return invoices, err
		}

		invoiceID := addInvoiceResponse.PaymentRequest
		hash := addInvoiceResponse.RHash
		expirationTime := time.Now().Add(time.Minute * 59)
		i := Invoice{PaymentRequest: invoiceID, Settled: false, PaymentHash: hash, Client: c, ExpirationTime: expirationTime}
		invoices = append(invoices, &i)
		err = i.save()
		if err != nil {
			// Couldn't save the invoice, so we will not keep it in store
			continue
		}
		c.Invoices[invoiceID] = &i
	}

	return invoices, nil
}

func discreteTypeValidator(c *Client, w http.ResponseWriter, r *http.Request, handler func(http.ResponseWriter, *http.Request)) {

	invoiceID := readHeader(r.Header, "Light-Auth-Invoice")
	if invoiceID == "" {
		http.Error(w, mISSINGINVOICE, http.StatusBadRequest)
		return
	}

	preImageString := readHeader(r.Header, "Light-Auth-Pre-Image")
	if preImageString == "" {
		http.Error(w, mISSINGPREIMAGE, http.StatusBadRequest)
		return
	}

	i, invoiceExists := c.Invoices[invoiceID]
	if !invoiceExists {
		http.Error(w, iNVALIDCREDENTIALS, http.StatusBadRequest)
		return
	}

	preImage, err := hex.DecodeString(preImageString)
	if err != nil {
		http.Error(w, iNVALIDCREDENTIALS, http.StatusBadRequest)
		return
	}
	hasher := sha256.New()
	hasher.Write(preImage)
	hexPreImage := hex.EncodeToString(hasher.Sum(nil))
	hexPaymentHash := hex.EncodeToString(i.PaymentHash)

	if hexPreImage != hexPaymentHash {
		http.Error(w, iNVALIDCREDENTIALS, http.StatusBadRequest)
		return
	}

	if i.isClaimed() {
		http.Error(w, iNVOICEALREADYCLAIMED, http.StatusBadRequest)
	}

	if !i.isSettled() {
		http.Error(w, tRYAGAIN, http.StatusConflict)
		return
	}

	err = i.claim()
	if err != nil {
		http.Error(w, sOMETHINGWENTWRONG, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Light-Auth-Invoice", invoiceID)

	handler(w, r)
}

func timeTypeValidator(c *Client, w http.ResponseWriter, r *http.Request, handler func(http.ResponseWriter, *http.Request)) {
	t := time.Now()
	expired := c.ExpirationTime.Before(t)
	if expired {
		http.Error(w, tIMEEXPIRED, http.StatusPaymentRequired)
		return
	}

	handler(w, r)
}

// ServerMiddleware is a middleware that checks if the request is valid according to the fees declared for the
// route.
func ServerMiddleware(handler func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		routeName := r.Method + r.URL.Path
		rt, routeExists := serverStore[routeName]
		if !routeExists {
			handler(w, r)
			return
		}

		token := readHeader(r.Header, "Light-Auth-Token")
		if token == "" {
			for {
				// Token not found, create new one
				if _, tokenExists := rt.Clients[token]; !tokenExists {
					token = uniuri.New()
					c := &Client{Token: token, Invoices: map[string]*Invoice{}, ExpirationTime: time.Now(), Route: rt}
					err := c.save()
					if err != nil {
						log.Printf("Lightauth error: Could not save client: %v\n", err)
						http.Error(w, "Something went wrong", http.StatusInternalServerError)
						return
					}
					rt.Clients[token] = c
					break
				}
			}
		}

		writeConstantHeaders(w, rt.RouteInfo)

		_, tokenExists := rt.Clients[token]
		if !tokenExists {
			// Token doesn't exist
			http.Error(w, iNVALIDTOKEN, http.StatusBadRequest)
			return
		}

		var err error
		c := rt.Clients[token]
		err = writeClientHeaders(w, c)
		if err != nil {
			return
		}

		if rt.Mode == "time" {
			timeTypeValidator(c, w, r, handler)
		} else if rt.Mode == "discrete" {
			discreteTypeValidator(c, w, r, handler)
		}
	}
}
