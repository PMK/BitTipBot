package lnbits

import (
	"fmt"
	"time"

	"github.com/imroc/req"
)

// NewClient returns a new lnbits api client. Pass your API key and url here.
func NewClient(key, url string) *Client {
	return &Client{
		url: url,
		// info: this header holds the ADMIN key for the entire API
		// it can be used to create wallets for example
		// if you want to check the balance of a user, use w.Inkey
		// if you want to make a payment, use w.Adminkey
		header: req.Header{
			"Content-Type": "application/json",
			"Accept":       "application/json",
			"X-Api-Key":    key,
		},
	}
}

// CreateAccount creates a new LNbits account with a wallet using the native
// v1 API endpoint POST /api/v1/account. This replaces the deprecated User
// Manager extension. The response includes wallet keys directly.
func (c *Client) CreateAccount(walletName string) (wal Wallet, err error) {
	resp, err := req.Post(
		c.url+"/api/v1/account",
		c.header,
		req.BodyJSON(struct {
			Name string `json:"name"`
		}{Name: walletName}),
	)
	if err != nil {
		return
	}

	if resp.Response().StatusCode >= 300 {
		var reqErr Error
		resp.ToJSON(&reqErr)
		err = reqErr
		return
	}
	err = resp.ToJSON(&wal)
	return
}

// Invoice creates an invoice associated with this wallet.
func (w Wallet) Invoice(params InvoiceParams, c *Client) (lntx Invoice, err error) {
	// custom header with invoice key
	invoiceHeader := req.Header{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"X-Api-Key":    w.Inkey,
	}
	resp, err := req.Post(c.url+"/api/v1/payments", invoiceHeader, req.BodyJSON(&params))
	if err != nil {
		return
	}

	if resp.Response().StatusCode >= 300 {
		var reqErr Error
		resp.ToJSON(&reqErr)
		err = reqErr
		return
	}

	err = resp.ToJSON(&lntx)
	return
}

// InvoiceForWallet creates an invoice for a specific wallet using that
// wallet's inkey. The walletInkey authenticates the request so the invoice
// is created for (and credited to) the correct wallet.
func (c *Client) InvoiceForWallet(walletInkey string, params InvoiceParams) (Invoice, error) {
	params.Out = false
	var result Invoice
	walletHeader := req.Header{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"X-Api-Key":    walletInkey,
	}
	resp, err := req.Post(c.url+"/api/v1/payments", walletHeader, req.BodyJSON(&params))
	if err != nil {
		return result, err
	}
	if resp.Response().StatusCode >= 300 {
		var reqErr Error
		resp.ToJSON(&reqErr)
		return result, reqErr
	}
	err = resp.ToJSON(&result)
	return result, err
}

// Info returns wallet information
func (c Client) Info(w Wallet) (wtx Wallet, err error) {
	// custom header with invoice key
	invoiceHeader := req.Header{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"X-Api-Key":    w.Inkey,
	}
	resp, err := req.Get(c.url+"/api/v1/wallet", invoiceHeader, nil)
	if err != nil {
		return
	}

	if resp.Response().StatusCode >= 300 {
		var reqErr Error
		resp.ToJSON(&reqErr)
		err = reqErr
		return
	}

	err = resp.ToJSON(&wtx)
	return
}

// Payments returns wallet payments
func (c Client) Payments(w Wallet) (wtx Payments, err error) {
	// custom header with invoice key
	invoiceHeader := req.Header{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"X-Api-Key":    w.Inkey,
	}
	resp, err := req.Get(c.url+"/api/v1/payments?limit=60", invoiceHeader, nil)
	if err != nil {
		return
	}

	if resp.Response().StatusCode >= 300 {
		var reqErr Error
		resp.ToJSON(&reqErr)
		err = reqErr
		return
	}

	err = resp.ToJSON(&wtx)
	return
}

// Payment state of a payment
func (c Client) Payment(w Wallet, payment_hash string) (payment LNbitsPayment, err error) {
	// custom header with invoice key
	invoiceHeader := req.Header{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"X-Api-Key":    w.Inkey,
	}
	resp, err := req.Get(c.url+fmt.Sprintf("/api/v1/payments/%s", payment_hash), invoiceHeader, nil)
	if err != nil {
		return
	}

	if resp.Response().StatusCode >= 300 {
		var reqErr Error
		resp.ToJSON(&reqErr)
		err = reqErr
		return
	}

	err = resp.ToJSON(&payment)
	return
}

// Pay pays a given invoice with funds from the wallet.
func (w Wallet) Pay(params PaymentParams, c *Client) (wtx Invoice, err error) {
	// custom header with admin key
	adminHeader := req.Header{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"X-Api-Key":    w.Adminkey,
	}
	r := req.New()
	r.SetTimeout(time.Hour * 24)
	resp, err := r.Post(c.url+"/api/v1/payments", adminHeader, req.BodyJSON(&params))
	if err != nil {
		return
	}

	if resp.Response().StatusCode >= 300 {
		var reqErr Error
		resp.ToJSON(&reqErr)
		err = reqErr
		return
	}

	err = resp.ToJSON(&wtx)
	return
}
