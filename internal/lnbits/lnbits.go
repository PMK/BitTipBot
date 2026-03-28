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

// GetUser returns user information
// Updated to use the new LNBits Users API (no UserManager plugin).
func (c *Client) GetUser(userId string) (user User, err error) {
	// new Users API exposes users at GET /api/v1/users/{user_id}
	resp, err := req.Get(c.url+"/users/api/v1/user/"+userId, c.header, nil)
	if err != nil {
		return
	}

	if resp.Response().StatusCode >= 300 {
		var reqErr Error
		resp.ToJSON(&reqErr)
		err = reqErr
		return
	}

	err = resp.ToJSON(&user)
	return
}

// CreateUserWithInitialWallet creates a new LNBits user and an initial wallet.
// LNBits v1 requires two separate API calls: one to create the user and one to
// create the wallet. The admin key is passed via the X-Api-Key header.
func (c *Client) CreateUserWithInitialWallet(userName, walletName, adminId string, email string) (user User, err error) {
	// Step 1: Create the user account.
	resp, err := req.Post(c.url+"/users/api/v1/user", c.header, req.BodyJSON(struct {
		UserName string `json:"username"`
	}{userName}))
	if err != nil {
		return
	}
	if resp.Response().StatusCode >= 300 {
		var reqErr Error
		resp.ToJSON(&reqErr)
		err = reqErr
		return
	}
	// LNBits v1 returns "username", but our internal User struct uses json:"name".
	// Use an intermediate struct to bridge the mismatch.
	var apiResp struct {
		ID       string `json:"id"`
		UserName string `json:"username"`
	}
	if err = resp.ToJSON(&apiResp); err != nil {
		return
	}
	user.ID = apiResp.ID
	user.Name = apiResp.UserName

	// Step 2: Create the initial wallet for the new user.
	_, err = c.CreateWallet(user.ID, walletName, adminId)
	return
}

// CreateWallet creates a new wallet for an existing LNBits user.
// LNBits v1 endpoint: POST /users/api/v1/user/{user_id}/wallet
func (c *Client) CreateWallet(userId, walletName, adminId string) (wal Wallet, err error) {
	resp, err := req.Post(c.url+"/users/api/v1/user/"+userId+"/wallet", c.header, req.BodyJSON(struct {
		Name string `json:"name"`
	}{walletName}))
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

// Wallets returns all wallets belonging to a user.
// LNBits v1 endpoint: GET /users/api/v1/user/{user_id}/wallet
func (c Client) Wallets(u User) (wtx []Wallet, err error) {
	resp, err := req.Get(c.url+"/users/api/v1/user/"+u.ID+"/wallet", c.header, nil)
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
