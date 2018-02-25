package lightauth

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dchest/uniuri"
	"github.com/lightningnetwork/lnd/lnrpc"
)

const (
	iNVALIDTOKEN       = "Lightauth error: Invalid token"
	tIMEEXPIRED        = "Lightauth error: Your authorized time has expired, pay up some balances to buy more time"
	iNVALIDCREDENTIALS = "Lightauth error: Invalid credentials"
	mISSINGINVOICE     = "Lightauth error: Missing invoice ID"
	mISSINGPREIMAGE    = "Lightauth error: Missing pre_image"
	tRYAGAIN           = "Lightauth error: We can't validate your payment yet, please try again"
)

type route struct {
	routeInfo
	clients map[string]*client
}

type invoice struct {
	paymentRequest string
	fee            int
	settled        bool
	preImage       string
	claimed        bool
	mux            sync.Mutex
}

type client struct {
	token    string
	expires  time.Time
	invoices map[string]*invoice
	route    route
}

func writeConstantHeaders(w http.ResponseWriter, rt routeInfo) {
	w.Header().Set("Light-Auth-Name", rt.Name)
	w.Header().Set("Light-Auth-Mode", rt.Mode)
	w.Header().Set("Light-Auth-Fee", strconv.Itoa(rt.Fee))
	w.Header().Set("Light-Auth-Max-Invoices", strconv.Itoa(rt.MaxInvoices))

	if rt.Mode == "time" {
		w.Header().Set("Light-Auth-Time-Period", rt.Period)
	}
}

func writeClientHeaders(w http.ResponseWriter, c *client) error {
	unpayedInvoices, err := c.getUnpayedInvoices()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}

	unpayedInvoicesRequests := []string{}
	for _, v := range unpayedInvoices {
		unpayedInvoicesRequests = append(unpayedInvoicesRequests, v)
	}

	w.Header().Set("Light-Auth-Token", c.token)
	w.Header().Set("Light-Auth-Invoices", strings.Join(unpayedInvoicesRequests, ","))

	if c.route.Mode == "time" {
		// RFC3339
		w.Header().Set("Light-Auth-Expiration-Time", c.expires.Format("2006-01-02T15:04:05Z07:00"))
	}

	return err
}

func updateInvoice(paymentRequest string) {
	for _, r := range serverStore {
		for _, c := range r.clients {
			if i, invoiceExists := c.invoices[paymentRequest]; invoiceExists {
				i.mux.Lock()
				i.settled = true
				i.mux.Unlock()

				if c.route.Mode == "time" {
					timePeriod := time.Millisecond
					switch c.route.Period {
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
					if c.expires.After(t) {
						diff := c.expires.Sub(t)
						c.expires = t.Add(timePeriod).Add(diff)
					} else {
						c.expires = t.Add(timePeriod)
					}
				}
			}
		}
	}
}

func (c *client) getUnpayedInvoices() ([]string, error) {
	unpayedInvoices := []string{}
	for _, i := range c.invoices {
		i.mux.Lock()
		if !i.settled {
			unpayedInvoices = append(unpayedInvoices, i.paymentRequest)

		}
		i.mux.Unlock()
	}

	numUnpayed := len(unpayedInvoices)
	if numUnpayed < c.route.MaxInvoices {
		newInvoices, err := c.generateInvoices(c.route.MaxInvoices - numUnpayed)
		if err != nil {
			return []string{}, err
		}

		unpayedInvoices = append(unpayedInvoices, newInvoices...)
	}

	return unpayedInvoices, nil
}

func (c *client) generateInvoices(numberOfInvoices int) ([]string, error) {
	ctxb := context.Background()
	invoices := []string{}

	for i := 0; i < numberOfInvoices; i++ {
		addInvoiceResponse, err := lightningClient.AddInvoice(ctxb, &lnrpc.Invoice{Value: int64(c.route.Fee)})
		if err != nil {
			log.Fatalf("error %v", err)
			return invoices, err
		}

		invoiceID := addInvoiceResponse.PaymentRequest
		i := invoice{paymentRequest: invoiceID, settled: false}
		invoices = append(invoices, i.paymentRequest)
		c.invoices[invoiceID] = &i
	}

	return invoices, nil
}

func discreteTypeValidator(c *client, w http.ResponseWriter, r *http.Request, handler func(http.ResponseWriter, *http.Request)) {

	invoiceID := readHeader(r.Header, "Light-Auth-Invoice")
	if invoiceID == "" {
		http.Error(w, mISSINGINVOICE, http.StatusBadRequest)
		return
	}

	preImage := readHeader(r.Header, "Light-Auth-Pre-Image")
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

	i.mux.Lock()
	settled := i.settled
	i.mux.Unlock()

	if !settled {
		http.Error(w, tRYAGAIN, http.StatusConflict)
		return
	}

	handler(w, r)
}

func timeTypeValidator(c *client, w http.ResponseWriter, r *http.Request, handler func(http.ResponseWriter, *http.Request)) {
	t := time.Now()
	expired := c.expires.Before(t)
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
			// Response not configured, log and pass control to handler.
			handler(w, r)
			return
		}

		token := readHeader(r.Header, "Light-Auth-Token")
		if token == "" {
			for {
				// Token not found, create new one
				if _, tokenExists := rt.clients[token]; !tokenExists {
					token = uniuri.New()
					rt.clients[token] = &client{token: token, invoices: map[string]*invoice{}, expires: time.Now(), route: *rt}
					break
				}
			}
		}

		writeConstantHeaders(w, rt.routeInfo)

		_, tokenExists := rt.clients[token]
		if !tokenExists {
			// Token doesn't exist
			http.Error(w, iNVALIDTOKEN, http.StatusBadRequest)
			return
		}

		var err error
		c := rt.clients[token]
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
