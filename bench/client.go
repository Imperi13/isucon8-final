package bench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ken39arg/isucon2018-final/bench/urlcache"

	"github.com/pkg/errors"
	"golang.org/x/net/publicsuffix"
)

const (
	UserAgent     = "Isutrader/0.0.1"
	TradeTypeSell = "sell"
	TradeTypeBuy  = "buy"
)

var (
	ErrAlreadyRetired = errors.New("alreay retired client")
)

type ResponseWithElapsedTime struct {
	*http.Response
	ElapsedTime time.Duration
}

type ErrElapsedTimeOverRetire struct {
	s string
}

func (e *ErrElapsedTimeOverRetire) Error() string {
	return e.s
}

type ErrorWithStatus struct {
	StatusCode int
	Body       string
	err        error
}

func errorWithStatus(err error, code int, body string) *ErrorWithStatus {
	return &ErrorWithStatus{
		StatusCode: code,
		Body:       body,
		err:        err,
	}
}

func (e *ErrorWithStatus) Error() string {
	return fmt.Sprintf("%s [status:%d, body:%s]", e.err.Error(), e.StatusCode, e.Body)
}

type StatusRes struct {
	OK    bool   `jon:"ok"`
	Error string `jon:"error,omitempty"`
}

type User struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	BankID    string    `json:"-"`
	CreatedAt time.Time `json:"-"`
}

type Trade struct {
	ID        int64     `json:"id"`
	Amount    int64     `json:"amount"`
	Price     int64     `json:"price"`
	CreatedAt time.Time `json:"created_at"`
}

type Order struct {
	ID        int64      `json:"id"`
	Type      string     `json:"type"`
	UserID    int64      `json:"user_id"`
	Amount    int64      `json:"amount"`
	Price     int64      `json:"price"`
	ClosedAt  *time.Time `json:"closed_at"`
	TradeID   int64      `json:"trade_id,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	User      *User      `json:"user,omitempty"`
	Trade     *Trade     `json:"trade,omitempty"`
}

type CandlestickData struct {
	Time  time.Time `json:"time"`
	Open  int64     `json:"open"`
	Close int64     `json:"close"`
	High  int64     `json:"high"`
	Low   int64     `json:"low"`
}

type InfoResponse struct {
	Cursor          int64             `json:"cursor"`
	TradedOrders    []Order           `json:"traded_orders"`
	LowestSellPrice int64             `json:"lowest_sell_price"`
	HighestBuyPrice int64             `json:"highest_buy_price"`
	ChartBySec      []CandlestickData `json:"chart_by_sec"`
	ChartByMin      []CandlestickData `json:"chart_by_min"`
	ChartByHour     []CandlestickData `json:"chart_by_hour"`
}

type OrderActionResponse struct {
	ID int64 `json:"id"`
}

type Client struct {
	base     *url.URL
	hc       *http.Client
	userID   int64
	bankid   string
	pass     string
	name     string
	cache    *urlcache.CacheStore
	retired  bool
	retireto time.Duration
}

func NewClient(base, bankid, name, password string, timout, retire time.Duration) (*Client, error) {
	b, err := url.Parse(base)
	if err != nil {
		return nil, errors.Wrapf(err, "base url parse Failed.")
	}
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, errors.Wrapf(err, "cookiejar.New Failed.")
	}
	hc := &http.Client{
		Jar: jar,
		// Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: timout,
	}
	return &Client{
		base:     b,
		hc:       hc,
		bankid:   bankid,
		name:     name,
		pass:     password,
		cache:    urlcache.NewCacheStore(),
		retireto: retire,
	}, nil
}

func (c *Client) IsRetired() bool {
	return c.retired
}

func (c *Client) UserID() int64 {
	return c.userID
}

func (c *Client) doRequest(req *http.Request) (*ResponseWithElapsedTime, error) {
	if c.retired {
		return nil, ErrAlreadyRetired
	}
	req.Header.Set("User-Agent", UserAgent)
	var reqbody []byte
	if req.Body != nil {
		var err error
		reqbody, err = ioutil.ReadAll(req.Body) // for retry
		if err != nil {
			return nil, errors.Wrapf(err, "reqbody read faild")
		}
	}
	start := time.Now()
	for {
		if reqbody != nil {
			req.Body = ioutil.NopCloser(bytes.NewBuffer(reqbody))
		}
		res, err := c.hc.Do(req)
		if err != nil {
			elapsedTime := time.Now().Sub(start)
			if e, ok := err.(*url.Error); ok {
				if e.Timeout() {
					c.retired = true
					return nil, &ErrElapsedTimeOverRetire{e.Error()}
				}
			}
			log.Printf("[WARN] err: %s, [%.5f] req.len:%d", err, elapsedTime.Seconds(), req.ContentLength)
			if elapsedTime < c.retireto {
				continue
			}
			return nil, err
		}
		elapsedTime := time.Now().Sub(start)
		if c.retireto < elapsedTime {
			if err = res.Body.Close(); err != nil {
				log.Printf("[WARN] body close failed. %s", err)
			}
			c.retired = true
			return nil, &ErrElapsedTimeOverRetire{
				s: fmt.Sprintf("this user gave up browsing because response time is too long. [%.5f s]", elapsedTime.Seconds()),
			}
		}
		if res.StatusCode < 500 {
			return &ResponseWithElapsedTime{res, elapsedTime}, nil
		}
		body, err := ioutil.ReadAll(res.Body)
		if err != nil {
			log.Printf("[INFO] retry status code: %d, read body failed: %s", res.StatusCode, err)
		} else {
			log.Printf("[INFO] retry status code: %d, body: %s", res.StatusCode, string(body))
		}
		time.Sleep(RetryInterval)
	}
}

func (c *Client) get(path string, val url.Values) (*ResponseWithElapsedTime, error) {
	u, err := c.base.Parse(path)
	if err != nil {
		return nil, errors.Wrap(err, "url parse failed")
	}
	for k, v := range u.Query() {
		val[k] = v
	}
	u.RawQuery = val.Encode()
	us := u.String()
	req, err := http.NewRequest(http.MethodGet, us, nil)
	if err != nil {
		return nil, errors.Wrap(err, "new request failed")
	}
	if cache, found := c.cache.Get(us); found {
		// no-storeを外しかつcache-controlをつければOK
		// if cache.CanUseCache() {
		// 	return &ResponseWithElapsedTime{
		// 		Response: &http.Response{
		// 			StatusCode: http.StatusNotModified,
		// 			Body:       ioutil.NopCloser(&bytes.Buffer{}),
		// 		},
		// 		ElapsedTime: 0,
		// 	}, nil
		// }
		cache.ApplyRequest(req)
	}
	res, err := c.doRequest(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode == 200 {
		body := &bytes.Buffer{}
		if _, err = io.Copy(body, res.Body); err != nil {
			return nil, err
		}
		if cache, _ := urlcache.NewURLCache(res.Response, body); cache != nil {
			c.cache.Set(us, cache)
		}
		res.Body = ioutil.NopCloser(body)
	}
	return res, nil
}

func (c *Client) post(path string, val url.Values) (*ResponseWithElapsedTime, error) {
	u, err := c.base.Parse(path)
	if err != nil {
		return nil, errors.Wrap(err, "url parse failed")
	}
	req, err := http.NewRequest(http.MethodPost, u.String(), strings.NewReader(val.Encode()))
	if err != nil {
		return nil, errors.Wrap(err, "new request failed")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.doRequest(req)
}

func (c *Client) del(path string, val url.Values) (*ResponseWithElapsedTime, error) {
	u, err := c.base.Parse(path)
	if err != nil {
		return nil, errors.Wrap(err, "url parse failed")
	}
	for k, v := range u.Query() {
		val[k] = v
	}
	u.RawQuery = val.Encode()
	req, err := http.NewRequest(http.MethodDelete, u.String(), nil)
	if err != nil {
		return nil, errors.Wrap(err, "new request failed")
	}
	return c.doRequest(req)
}

func (c *Client) Initialize(bankep, bankid, logep, logid string) error {
	v := url.Values{}
	v.Set("bank_endpoint", bankep)
	v.Set("bank_appid", bankid)
	v.Set("log_endpoint", logep)
	v.Set("log_appid", logid)
	res, err := c.post("/initialize", v)
	if err != nil {
		return errors.Wrap(err, "POST /initialize request failed")
	}
	defer res.Body.Close()
	b, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return errors.Wrap(err, "POST /initialize body read failed")
	}
	if res.StatusCode == http.StatusOK {
		return nil
	}
	return errorWithStatus(errors.Errorf("POST /initialize failed."), res.StatusCode, string(b))
}

func (c *Client) Signup() error {
	v := url.Values{}
	v.Set("name", c.name)
	v.Set("bank_id", c.bankid)
	v.Set("password", c.pass)
	res, err := c.post("/signup", v)
	if err != nil {
		if err == ErrAlreadyRetired {
			return err
		}
		return errors.Wrap(err, "POST /signup request failed")
	}
	defer res.Body.Close()
	b, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return errors.Wrap(err, "POST /signup body read failed")
	}
	if res.StatusCode == http.StatusOK {
		return nil
	}
	return errorWithStatus(errors.Errorf("POST /signup failed."), res.StatusCode, string(b))
}

func (c *Client) Signin() error {
	v := url.Values{}
	v.Set("bank_id", c.bankid)
	v.Set("password", c.pass)
	res, err := c.post("/signin", v)
	if err != nil {
		if err == ErrAlreadyRetired {
			return err
		}
		return errors.Wrap(err, "POST /signin request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return errors.Wrapf(err, "POST /signin body read failed")
		}
		return errorWithStatus(errors.Errorf("POST /signin failed."), res.StatusCode, string(b))
	}
	r := &User{}
	if err := json.NewDecoder(res.Body).Decode(r); err != nil {
		return errors.Wrapf(err, "POST /signin body decode failed")
	}
	if r.Name != c.name {
		return errors.Errorf("POST /signin returned different name [%s] my name is [%s]", r.Name, c.name)
	}
	if r.ID == 0 {
		return errors.Errorf("POST /signin returned zero id")
	}
	c.userID = r.ID
	return nil
}

func (c *Client) Signout() error {
	res, err := c.post("/signout", url.Values{})
	if err != nil {
		if err == ErrAlreadyRetired {
			return err
		}
		return errors.Wrap(err, "POST /signout request failed")
	}
	defer res.Body.Close()
	b, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return errors.Wrap(err, "POST /signout body read failed")
	}
	if res.StatusCode == http.StatusOK {
		return nil
	}
	return errorWithStatus(errors.Errorf("POST /signout failed."), res.StatusCode, string(b))
}

func (c *Client) Top() error {
	for _, path := range []string{
		"/",
		// TODO static files
		"/css/bootstrap-grid.min.css",
		"/css/bootstrap-reboot.min.css",
		"/css/bootstrap.min.css",
		"/js/bootstrap.bundle.min.js",
		"/js/bootstrap.min.js",
		"/js/jquery-3.3.1.slim.min.js",
		"/js/popper.min.js",
	} {
		err := func(path string) error {
			res, err := c.get(path, url.Values{})
			if err != nil {
				if err == ErrAlreadyRetired {
					return err
				}
				return errors.Wrapf(err, "GET %s request failed", path)
			}
			defer res.Body.Close()
			b, err := ioutil.ReadAll(res.Body)
			if err != nil {
				return errors.Wrapf(err, "GET %s body read failed", path)
			}
			if res.StatusCode >= 400 {
				return errorWithStatus(errors.Errorf("GET %s failed.", path), res.StatusCode, string(b))
			}
			// TODO MD5のチェック
			return nil
		}(path)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) Info(cursor int64) (*InfoResponse, error) {
	path := "/info"
	v := url.Values{}
	v.Set("cursor", strconv.FormatInt(cursor, 10))
	res, err := c.get(path, v)
	if err != nil {
		if err == ErrAlreadyRetired {
			return nil, err
		}
		return nil, errors.Wrapf(err, "GET %s request failed", path)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return nil, errors.Wrapf(err, "GET %s body read failed", path)
		}
		return nil, errorWithStatus(errors.Errorf("GET %s failed.", path), res.StatusCode, string(b))
	}
	r := &InfoResponse{}
	if err := json.NewDecoder(res.Body).Decode(r); err != nil {
		return nil, errors.Wrapf(err, "GET %s body decode failed", path)
	}
	return r, nil
}

func (c *Client) AddOrder(ordertyp string, amount, price int64) (*Order, error) {
	path := "/orders"
	v := url.Values{}
	v.Set("type", ordertyp)
	v.Set("amount", strconv.FormatInt(amount, 10))
	v.Set("price", strconv.FormatInt(price, 10))
	res, err := c.post(path, v)
	if err != nil {
		if err == ErrAlreadyRetired {
			return nil, err
		}
		return nil, errors.Wrapf(err, "POST %s request failed", path)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return nil, errors.Wrapf(err, "POST %s body read failed", path)
		}
		return nil, errorWithStatus(errors.Errorf("POST %s failed.", path), res.StatusCode, string(b))
	}
	r := &OrderActionResponse{}
	if err := json.NewDecoder(res.Body).Decode(r); err != nil {
		return nil, errors.Wrapf(err, "POST %s body decode failed", path)
	}
	if r.ID == 0 {
		return nil, errors.Errorf("POST %s failed. id is not returned", path)
	}

	return &Order{
		ID:     r.ID,
		Amount: amount,
		Price:  price,
		Type:   ordertyp,
	}, nil
}

func (c *Client) GetOrders() ([]Order, error) {
	path := "/orders"
	res, err := c.get(path, url.Values{})
	if err != nil {
		if err == ErrAlreadyRetired {
			return nil, err
		}
		return nil, errors.Wrapf(err, "GET %s request failed", path)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return nil, errors.Wrapf(err, "GET %s body read failed", path)
		}
		return nil, errorWithStatus(errors.Errorf("GET %s failed.", path), res.StatusCode, string(b))
	}
	orders := []Order{}
	if err := json.NewDecoder(res.Body).Decode(&orders); err != nil {
		return nil, errors.Wrapf(err, "GET %s body decode failed", path)
	}
	for _, order := range orders {
		if order.UserID != c.userID {
			return nil, errors.Wrapf(err, "GET %s returned not my order [id:%d, user_id:%d]", path, order.ID, c.UserID)
		}
		if order.User == nil {
			return nil, errors.Wrapf(err, "GET %s returned not filled user [id:%d, user_id:%d]", path, order.ID, c.UserID)
		}
		if order.User.Name != c.name {
			return nil, errors.Wrapf(err, "GET %s returned filled user.name is not my name [id:%d, user_id:%d]", path, order.ID, c.UserID)
		}
		if order.TradeID != 0 && order.Trade == nil {
			return nil, errors.Wrapf(err, "GET %s returned not filled trade [id:%d, user_id:%d]", path, order.ID, c.UserID)
		}
	}
	return orders, nil
}

func (c *Client) DeleteOrders(id int64) error {
	path := fmt.Sprintf("/order/%d", id)
	res, err := c.del(path, url.Values{})
	if err != nil {
		if err == ErrAlreadyRetired {
			return err
		}
		return errors.Wrapf(err, "DELETE %s request failed", path)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return errors.Wrapf(err, "DELETE %s body read failed", path)
		}
		return errorWithStatus(errors.Errorf("DELETE %s failed.", path), res.StatusCode, string(b))
	}
	r := &OrderActionResponse{}
	if err := json.NewDecoder(res.Body).Decode(r); err != nil {
		return errors.Wrapf(err, "DELETE %s body decode failed", path)
	}
	if r.ID != id {
		return errors.Errorf("DELETE %s failed. id is not match requested value [got:%d, want:%d]", path, r.ID, id)
	}
	return nil
}
