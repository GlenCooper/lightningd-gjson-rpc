package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/fiatjaf/go-lnurl"
	"github.com/fiatjaf/lightningd-gjson-rpc/plugin"
	"github.com/tidwall/gjson"
)

func main() {
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	http.DefaultTransport.(*http.Transport).Proxy = http.ProxyFromEnvironment

	p := plugin.Plugin{
		Name:    "lnurl",
		Version: "v1.0",
		RPCMethods: []plugin.RPCMethod{
			{
				"lnurlencode",
				"url",
				"Encode an URL into bech32.",
				"",
				func(p *plugin.Plugin, params plugin.Params) (resp interface{}, errCode int, err error) {
					url, _ := params["url"].(string)
					encodedlnurl, err := lnurl.LNURLEncode(url)
					if err != nil {
						return nil, 500, err
					}

					return map[string]interface{}{"lnurl": encodedlnurl}, 0, nil
				},
			}, {
				"lnurldecode",
				"lnurl",
				"Decode a bech32-encoded lnurl into an URL.",
				"",
				func(p *plugin.Plugin, params plugin.Params) (resp interface{}, errCode int, err error) {
					lnurltext, _ := params["lnurl"].(string)
					decodedlnurl, err := lnurl.LNURLDecode(lnurltext)
					if err != nil {
						return nil, 500, err
					}

					return map[string]interface{}{"url": decodedlnurl}, 0, nil
				},
			}, {
				"lnurlinvoice",
				"msatoshi label [description_hash] [metadata] [expiry] [preimage]",
				"Create an lnurl-pay invoice for {msatoshi} with {label} and either {metadata} or {description_hash} directly with optional {expiry} seconds (default 1 week) and optional {preimage} (default autogenerated)",
				"",
				func(p *plugin.Plugin, params plugin.Params) (resp interface{}, errCode int, err error) {
					msatoshi, err := params.Int("msatoshi")
					if err != nil {
						return nil, 400, errors.New("invalid msatoshi value")
					}

					expiry, err := params.Int("expiry")
					if err != nil {
						expiry = 60 * 60 * 24 * 7
					}
					dexpiry := time.Second * time.Duration(expiry)

					preimage, err := params.String("preimage")
					if err != nil {
						preimage = lnurl.RandomK1()
					}
					bpreimage, _ := hex.DecodeString(preimage)

					var descriptionHash []byte
					if dhash, err := params.String("description_hash"); err == nil {
						descriptionHash, _ = hex.DecodeString(dhash)
					} else {
						if metadata, err := params.String("metadata"); err == nil {
							bdhash := sha256.Sum256([]byte(metadata))
							descriptionHash = bdhash[:]
						}
					}
					if len(descriptionHash) != 32 {
						return nil, 477, errors.New("description_hash is invalid or not given, or metadata not given or passed with the wrong type")
					}

					newinv, err := p.Client.InvoiceWithDescriptionHash(
						params["label"].(string),
						int64(msatoshi),
						descriptionHash,
						&bpreimage,
						&dexpiry,
					)
					if err != nil {
						return nil, 500, errors.New("error making invoice on lightningd: " + err.Error())
					}

					return map[string]interface{}{
						"bolt11":           newinv,
						"description_hash": hex.EncodeToString(descriptionHash),
						"preimage":         preimage,
						"expires_at":       time.Now().Add(dexpiry).Unix(),
					}, 0, nil
				},
			}, {
				"lnurlparams",
				"lnurl",
				"Will fetch the params from the server or (when the decoded URL has a 'login' querystring) get then from the querystring, then return these params as JSON. {lnurl} is the bech32-encoded URL to query.",
				"",
				func(p *plugin.Plugin, params plugin.Params) (resp interface{}, errCode int, err error) {
					data, err := lnurl.HandleLNURL(params["lnurl"].(string))
					if err != nil {
						return nil, 401, err
					}
					return data, 0, nil
				},
			},
			{
				"lnurl",
				"lnurl [private] [msatoshi] [description]",
				"Will decode the lnurl, get its params (as in 'lnurlparams') and proceed with the lnurl flow according to the tag (login, withdraw etc.). {lnurl} is the bech32-encoded URL to query. {private} is either true or false, used on lnurl-channel for the type of channel (defaults to false). {description} is used on lnurl-withdraw (defaults to the default description). {msatoshi} is an integer, used on lnurl-withdraw and lnurl-pay (defaults to maximum possible amount).",
				"",
				func(p *plugin.Plugin, params plugin.Params) (resp interface{}, errCode int, err error) {
					data, err := lnurl.HandleLNURL(params["lnurl"].(string))
					if err != nil {
						return nil, 401, err
					}

					switch lnurlparams := data.(type) {
					case lnurl.LNURLChannelResponse:
						// connect to target node, notify it and wait for incoming channel.
						// no user interaction needed.
						private, _ := params["private"]
						var privateparam string
						switch priv := private.(type) {
						case bool:
							if priv {
								privateparam = "1"
							} else {
								privateparam = "0"
							}
						default:
							privateparam = "0"
						}

						_, err = p.Client.Call("connect", lnurlparams.URI)
						if err != nil {
							return nil, 204, err
						}

						respinfo, err := p.Client.Call("getinfo")
						if err != nil {
							return nil, 501, err
						}

						u, err := url.Parse(lnurlparams.Callback)
						if err != nil {
							return nil, 202, err
						}

						qs := u.Query()
						qs.Set("k1", lnurlparams.K1)
						qs.Set("private", privateparam)
						qs.Set("remoteid", respinfo.Get("id").String())
						u.RawQuery = qs.Encode()

						return callCallback(u, map[string]interface{}{
							"status":              "OK",
							"waiting_for_channel": true,
						})
					case lnurl.LNURLAuthParams:
						// sign k1 with linkingKey and send it along with key
						k1, err := hex.DecodeString(lnurlparams.K1)
						if err == nil && len(k1) != 32 {
							err = errors.New("Invalid length k1.")
						}
						if err != nil {
							return nil, 407, err
						}

						// parse callback url
						u, err := url.Parse(lnurlparams.Callback)
						if err != nil {
							return nil, 202, err
						}

						// get linking key for callback domain
						sk, err := getLinkingKey(p, u.Host)
						if err != nil {
							return nil, 500, err
						}

						// sign
						sig, err := sk.Sign(k1)
						if err != nil {
							return nil, 500, err
						}

						// call callback with signature and key
						signature := hex.EncodeToString(sig.Serialize())
						pubkey := hex.EncodeToString(sk.PubKey().SerializeCompressed())
						qs := u.Query()
						qs.Set("sig", signature)
						qs.Set("key", pubkey)
						u.RawQuery = qs.Encode()

						return callCallback(u, map[string]interface{}{
							"status":     "OK",
							"login":      true,
							"domain":     u.Host,
							"public_key": pubkey,
							"signature":  signature,
						})
					case lnurl.LNURLWithdrawResponse:
						// amount and description should be taken either from CLI params
						// or from interactive input.

						description := lnurlparams.DefaultDescription
						msatoshi := getBiggestIncomingCapacity(p)

						if msatoshi > lnurlparams.MaxWithdrawable {
							msatoshi = lnurlparams.MaxWithdrawable
						}

						idescription, ok := params["description"]
						if ok {
							descriptionparam, _ := idescription.(string)
							if descriptionparam != "" {
								description = descriptionparam
							}
						}

						imsatoshi, ok := params["msatoshi"]
						if ok {
							msatoshiparam, _ := imsatoshi.(int)
							if msatoshiparam != 0 {
								msatoshi = int64(msatoshiparam)

								// check msatoshi min/max
								if msatoshi > lnurlparams.MaxWithdrawable {
									return nil, 507, fmt.Errorf("msatoshi amount '%d' is bigger than the maximum specified by the server (%d).", msatoshi, lnurlparams.MaxWithdrawable)
								}
								if msatoshi > lnurlparams.MinWithdrawable {
									return nil, 507, fmt.Errorf("msatoshi amount '%d' is smaller than the minimum specified by the server (%d).", msatoshi, lnurlparams.MinWithdrawable)
								}
								if msatoshi > getBiggestIncomingCapacity(p) {
									return nil, 507, fmt.Errorf("msatoshi amount '%d' is bigger than the maximum we can receive (%d).", msatoshi, getBiggestIncomingCapacity(p))
								}
							}
						}

						// parse callback
						u, err := url.Parse(lnurlparams.Callback)
						if err != nil {
							return nil, 202, err
						}

						// generate bolt11 invoice
						label := fmt.Sprintf("lnurl-withdraw-%s-%s", u.Host, lnurlparams.K1)
						resp, err := p.Client.Call("invoice", msatoshi, label, description)
						bolt11 := resp.Get("bolt11").String()

						// call callback with bolt11 invoice and params
						qs := u.Query()
						qs.Set("k1", lnurlparams.K1)
						qs.Set("pr", bolt11)

						// only if there's a valid k1, sign it too
						if k1, err := hex.DecodeString(lnurlparams.K1); err == nil {
							if sk, err := getLinkingKey(p, u.Host); err == nil {
								if sig, err := sk.Sign(k1); err == nil {
									qs.Set("sig", hex.EncodeToString(sig.Serialize()))
								}
							}
						} // otherwise ignore.

						u.RawQuery = qs.Encode()

						return callCallback(u, map[string]interface{}{
							"status":              "OK",
							"waiting_for_payment": true,
							"bolt11":              bolt11,
							"label":               label,
						})
					case lnurl.LNURLPayResponse1:
					}

					return nil, 404, errors.New("unknown lnurl subprotocol")
				},
			},
		},
		Hooks: []plugin.Hook{
			{
				// a dirty hack to make spark-wallet pay to lnurl-pay endpoints
				"rpc_command",
				func(p *plugin.Plugin, params plugin.Params) (resp interface{}) {
					data := params.Get("rpc_command.rpc_command")

					switch data.Get("method").String() {
					case "decodepay":
						lnurltext, ok := lnurl.FindLNURLInText(
							data.Get("params.bolt11").String())
						if !ok {
							return map[string]interface{}{"continue": true}
						}

						// it's an lnurl
						iparams, err := lnurl.HandleLNURL(lnurltext)
						if err != nil {
							return map[string]interface{}{"continue": true}
						}

						params, ok := iparams.(lnurl.LNURLPayResponse1)
						if !ok {
							return map[string]interface{}{"continue": true}
						}

						urlp, err := url.Parse(params.Callback)
						if err != nil {
							return map[string]interface{}{"continue": true}
						}

						decoded := map[string]interface{}{
							"currency":   "bc",
							"created_at": time.Now().Unix(),
							"expiry":     1800,
							"description": "from " + urlp.Host + " \n\n" +
								params.ParsedMetadata["text/plain"],
						}

						if params.MinSendable == params.MaxSendable {
							decoded["msatoshi"] = params.MinSendable
						} else {
							decoded["description"] = decoded["description"].(string) +
								fmt.Sprintf(
									" \n\n(between %d and %d)",
									int64(params.MinSendable), int64(params.MaxSendable))
						}

						return map[string]interface{}{
							"return": map[string]interface{}{
								"result": decoded,
							},
						}
					case "pay":
					}

					return map[string]interface{}{"continue": true}
				},
			},
		},
		Dynamic: true,
	}

	p.Run()
}

func getBiggestIncomingCapacity(p *plugin.Plugin) (biggest int64) {
	resp, err := p.Client.Call("listfunds")
	if err != nil {
		return
	}

	resp.Get("channels").ForEach(func(_, value gjson.Result) bool {
		incoming := value.Get("channel_total_sat").Int() - value.Get("channel_sat").Int()
		if incoming > biggest {
			biggest = incoming
		}
		return true
	})

	return biggest
}

func callCallback(
	u *url.URL,
	jsonresponsesuccess map[string]interface{},
) (response interface{}, errCode int, err error) {
	resp, err := http.Get(u.String())
	if err != nil {
		return nil, 501, err
	}

	var lnurlresp lnurl.LNURLResponse
	err = json.NewDecoder(resp.Body).Decode(&lnurlresp)
	if err != nil {
		return nil, 205, err
	}

	if lnurlresp.Status == "ERROR" {
		return nil, 206, errors.New(lnurlresp.Reason)
	}

	return jsonresponsesuccess, 0, nil
}

func getLinkingKey(p *plugin.Plugin, domain string) (sk *btcec.PrivateKey, err error) {
	return p.Client.GetCustomKey(138, domain)
}
