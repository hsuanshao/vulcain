package gateway

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/gofrs/uuid"
	log "github.com/sirupsen/logrus"
)

var jsonRe = regexp.MustCompile(`(?i)\bjson\b`)

// Gateway is the main struct
type Gateway struct {
	options *Options
	server  *http.Server
	pushers *pushers
	openAPI *openAPI
}

func addToVary(r *http.Response, header string) {
	v := r.Header.Get("Vary")
	if v == "" {
		r.Header.Set("Vary", header)
		return
	}

	r.Header.Set("Vary", v+", "+header)
}

func extractFromRequest(req *http.Request) (fields, preload []string, fieldsHeader, fieldsQuery, preloadHeader, preloadQuery bool) {
	query := req.URL.Query()
	if len(req.Header["Fields"]) > 0 {
		fields = req.Header["Fields"]
		fieldsHeader = true
	} else if len(query["fields"]) > 0 {
		fields = query["fields"]
		fieldsQuery = true
	}

	if len(req.Header["Preload"]) > 0 {
		preload = req.Header["Preload"]
		preloadHeader = true
	} else if len(query["preload"]) > 0 {
		preload = query["preload"]
		preloadQuery = true
	}

	return fields, preload, fieldsHeader, fieldsQuery, preloadHeader, preloadQuery
}

func (g *Gateway) cleanupAfterRequest(p *waitPusher, explicitRequestID string, explicitRequest, wait bool) {
	if p == nil {
		return
	}

	if !explicitRequest {
		p.Done()
		return
	}

	if wait {
		// Wait for subrequests to finish
		p.Wait()
	}
	g.pushers.remove(explicitRequestID)
}

func (g *Gateway) getOpenAPIRoute(url *url.URL, route *openapi3filter.Route, routeTested bool) *openapi3filter.Route {
	if routeTested || g.openAPI == nil {
		return nil
	}

	return g.openAPI.getRoute(url)
}

func (g *Gateway) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	pusher, explicitRequest, explicitRequestID := g.getPusher(rw, req)

	rp := httputil.NewSingleHostReverseProxy(g.options.Upstream)
	rp.ModifyResponse = func(resp *http.Response) error {
		fields, preload, fieldsHeader, fieldsQuery, preloadHeader, preloadQuery := extractFromRequest(req)
		if (len(fields) == 0 && len(preload) == 0) || !jsonRe.MatchString(resp.Header.Get("Content-Type")) {
			// No Vulcain hints, or not JSON: don't modify the response
			g.cleanupAfterRequest(pusher, explicitRequestID, explicitRequest, false)
			return nil
		}

		currentBody, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		tree := &node{}
		tree.importPointers(Preload, preload)
		tree.importPointers(Fields, fields)

		var openAPIroute *openapi3filter.Route
		openAPIrouteTested := g.openAPI == nil
		newBody := traverseJSON(currentBody, tree, len(fields) > 0, func(n *node, v string) string {
			var (
				u          *url.URL
				useOpenAPI bool
				newValue   string
			)

			openAPIroute, openAPIrouteTested = g.getOpenAPIRoute(req.URL, openAPIroute, openAPIrouteTested), true
			if u, useOpenAPI, err = g.parseRelation(n.String(), v, openAPIroute); err != nil {
				return ""
			}

			// Never rewrite values when using OpenAPI, use header instead of query parameters
			if (preloadQuery || fieldsQuery) && !useOpenAPI {
				urlRewriter(u, n)
				newValue = u.String()
			}

			if len(preload) > 0 {
				g.push(u, pusher, req, resp, n, preloadHeader, fieldsHeader)
			}

			return newValue
		})

		if fieldsHeader {
			addToVary(resp, "Fields")
		}
		if preloadHeader {
			addToVary(resp, "Preload")
		}

		g.cleanupAfterRequest(pusher, explicitRequestID, explicitRequest, true)

		newBodyBuffer := bytes.NewBuffer(newBody)
		resp.Body = ioutil.NopCloser(newBodyBuffer)
		resp.Header["Content-Length"] = []string{fmt.Sprint(newBodyBuffer.Len())}

		return nil
	}
	rp.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
		// Adapted from the default ErrorHandler
		log.Errorf("http: proxy error: %v", err)
		rw.WriteHeader(http.StatusBadGateway)

		if pusher != nil && !explicitRequest {
			pusher.Done()
		}
	}
	rp.ServeHTTP(rw, req)
}

// addPreloadHeader sets preload Link headers as fallback when Server Push isn't available (https://www.w3.org/TR/preload/)
func addPreloadHeader(resp *http.Response, link string) {
	resp.Header.Add("Link", "<"+link+">; rel=preload; as=fetch")
	log.WithFields(log.Fields{"relation": link}).Debug("Link preload header added")
}

// TODO: allow to disable Server Push from the config
// TODO: allow to set the nopush attribute using the configuration (https://www.w3.org/TR/preload/#server-push-http-2)
// TODO: send 103 early hints responses (https://tools.ietf.org/html/rfc8297)
func (g *Gateway) push(u *url.URL, pusher *waitPusher, req *http.Request, resp *http.Response, n *node, preloadHeader, fieldsHeader bool) {
	url := u.String()
	if pusher == nil || u.IsAbs() {
		addPreloadHeader(resp, url)
		return
	}

	pushOptions := &http.PushOptions{Header: req.Header}
	pushOptions.Header.Del("Preload")
	pushOptions.Header.Del("Fields")
	pushOptions.Header.Del("Te") // Trailing headers aren't supported by Firefox for pushes, and we don't use them

	if preloadHeader {
		for _, pp := range n.strings(Preload, "") {
			if pp != "/" {
				pushOptions.Header.Add("Preload", pp)
			}
		}
	}
	if fieldsHeader {
		for _, fp := range n.strings(Fields, "") {
			if fp != "/" {
				pushOptions.Header.Add("Fields", fp)
			}
		}
	}

	// HTTP/2, and relative relation, push!
	if err := pusher.Push(url, pushOptions); err != nil {
		// Don't add the preload header for something already pushed
		if _, ok := err.(*relationAlreadyPushedError); !ok {
			log.WithFields(log.Fields{"relation": url, "reason": err.Error()}).Debug("Failed to push")
		}

		addPreloadHeader(resp, url)
		return
	}

	log.WithFields(log.Fields{"relation": url}).Debug("Relation pushed")
}

func (g *Gateway) getPusher(rw http.ResponseWriter, req *http.Request) (p *waitPusher, explicitRequest bool, explicitRequestID string) {
	internalPusher, ok := rw.(http.Pusher)
	if !ok {
		// Not an HTTP/2 connection
		return nil, false, ""
	}

	// Need https://github.com/golang/go/issues/20566 to get rid of this hack
	explicitRequestID = req.Header.Get("Vulcain-Explicit-Request")
	if explicitRequestID != "" {
		p, ok = g.pushers.get(explicitRequestID)
		if !ok {
			// Should not happen, is an attacker forging an evil request?
			log.WithFields(log.Fields{"uri": req.RequestURI, "explicitRequestID": explicitRequestID}).Debug("Pusher not found")
			explicitRequestID = ""
		}
	}

	if explicitRequestID == "" {
		// Explicit request
		explicitRequestID = uuid.Must(uuid.NewV4()).String()
		p = newWaitPusher(internalPusher, g.options.MaxPushes)
		req.Header.Set("Vulcain-Explicit-Request", explicitRequestID)
		g.pushers.add(explicitRequestID, p)

		return p, true, explicitRequestID
	}

	return p, false, explicitRequestID
}

// NewGatewayFromEnv creates a gateway using the configuration set in env vars
func NewGatewayFromEnv() (*Gateway, error) {
	options, err := NewOptionsFromEnv()
	if err != nil {
		return nil, err
	}

	return NewGateway(options), nil
}

// NewGateway creates a Vulcain gateway instance
func NewGateway(options *Options) *Gateway {
	var o *openAPI
	if options.OpenAPIFile != "" {
		o = newOpenAPI(options.OpenAPIFile)
	}

	return &Gateway{
		options,
		nil,
		&pushers{pusherMap: make(map[string]*waitPusher)},
		o,
	}
}

func (g *Gateway) parseRelation(selector, rel string, route *openapi3filter.Route) (*url.URL, bool, error) {
	var useOpenAPI bool
	if route != nil {
		if orel := g.openAPI.getRelation(route, selector, rel); orel != "" {
			rel = orel
			useOpenAPI = true
		}
	}

	u, err := url.Parse(rel)
	if err == nil {
		return u, useOpenAPI, nil
	}

	log.WithFields(
		log.Fields{
			"node":     selector,
			"relation": rel,
			"reason":   err,
		}).Debug("The URL generated using the OpenAPI specification is invalid")

	return nil, false, err
}
