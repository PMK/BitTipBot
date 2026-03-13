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

// GetUser returns user information from the LNbits Users extension API.
// Endpoint: GET /users/api/v1/user/{user_id}
func (c *Client) GetUser(userId string) (user User, err error) {
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

// lnbitsCreateUserRequest is the request body accepted by the LNbits v1 Users extension.
// The API key (admin key) goes in the X-Api-Key header, NOT in the body.
type lnbitsCreateUserRequest struct {
	UserName   string `json:"username"`    // LNbits v1 field name
	WalletName string `json:"wallet_name"` // name for the initial wallet
}

// lnbitsUserResponse is an intermediate struct that matches the LNbits v1 API
// response JSON exactly. The API returns "username" but our internal User struct
// uses json:"name" (to stay compatible with buntdb serialization throughout the
// codebase). We decode into this first, then map the fields to User.
type lnbitsUserResponse struct {
	ID       string `json:"id"`
	UserName string `json:"username"`
}

// CreateUserWithInitialWallet creates a new LNbits user with an initial wallet.
// It uses the Users extension endpoint POST /users/api/v1/user.
// The admin credentials are passed via the X-Api-Key header (already set on c.header).
func (c *Client) CreateUserWithInitialWallet(userName, walletName, adminId string, email string) (user User, err error) {
	resp, err := req.Post(
		c.url+"/users/api/v1/user",
		c.header,
		req.BodyJSON(lnbitsCreateUserRequest{
			UserName:   userName,
			WalletName: walletName,
		}),
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
	// Decode via the intermediate struct to handle the "username" vs "name" mismatch
	var apiResp lnbitsUserResponse
	if err = resp.ToJSON(&apiResp); err != nil {
		return
	}
	user.ID = apiResp.ID
	user.Name = apiResp.UserName
	return
}

// CreateWallet creates an additional wallet for an existing LNbits user.
// Endpoint: POST /users/api/v1/user/{user_id}/wallet
func (c *Client) CreateWallet(userId, walletName, adminId string) (wal Wallet, err error) {
	resp, err := req.Post(
		c.url+"/users/api/v1/user/"+userId+"/wallet",
		c.header,
		req.BodyJSON(struct {
			WalletName string `json:"wallet_name"`
		}{WalletName: walletName}),
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

// Invoice creates a BOLT11 invoice associated with this wallet.
func (w Wallet) Invoice(params InvoiceParams, c *Client) (lntx Invoice, err error) {
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

// Info returns wallet information (balance, keys, etc.).
func (c Client) Info(w Wallet) (wtx Wallet, err error) {
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

// Payments returns wallet payment history.
func (c Client) Payments(w Wallet) (wtx Payments, err error) {
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

// Payment returns the state of a single payment by hash.
func (c Client) Payment(w Wallet, payment_hash string) (payment LNbitsPayment, err error) {
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
// Uses the Users extension endpoint: GET /users/api/v1/user/{user_id}/wallets
func (c Client) Wallets(u User) (wtx []Wallet, err error) {
	resp, err := req.Get(c.url+"/users/api/v1/user/"+u.ID+"/wallets", c.header, nil)
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
