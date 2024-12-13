package restful

// Copyright 2013 Ernest Micklei. All rights reserved.
// Use of this source code is governed by a license
// that can be found in the LICENSE file.

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// CurlyRouter expects Routes with paths that contain zero or more parameters in curly brackets.
type CurlyRouter struct{}

// SelectRoute is part of the Router interface and returns the best match
// for the WebService and its Route for the given Request.
func (c CurlyRouter) SelectRoute(
	webServices []*WebService,
	httpRequest *http.Request) (selectedService *WebService, selected *Route, err error) {

	requestTokens := tokenizePath(httpRequest.URL.Path)

	detectedService := c.detectWebService(requestTokens, webServices)
	if detectedService == nil {
		if trace {
			traceLogger.Printf("no WebService was found to match URL path:%s\n", httpRequest.URL.Path)
		}
		return nil, nil, NewError(http.StatusNotFound, "404: Page Not Found")
	}
	candidateRoutes := c.selectRoutes(detectedService, requestTokens)
	if len(candidateRoutes) == 0 {
		if trace {
			traceLogger.Printf("no Route in WebService with path %s was found to match URL path:%s\n", detectedService.rootPath, httpRequest.URL.Path)
		}
		return detectedService, nil, NewError(http.StatusNotFound, "404: Page Not Found")
	}
	selectedRoute, err := c.detectRoute(candidateRoutes, httpRequest)
	if selectedRoute == nil {
		return detectedService, nil, err
	}
	return detectedService, selectedRoute, nil
}

// selectRoutes return a collection of Route from a WebService that matches the path tokens from the request.
func (c CurlyRouter) selectRoutes(ws *WebService, requestTokens []string) sortableCurlyRoutes {
	candidates := make(sortableCurlyRoutes, 0, 8)
	for _, eachRoute := range ws.routes {
		matches, paramCount, staticCount := c.matchesRouteByPathTokens(eachRoute.pathParts, requestTokens, eachRoute.hasCustomVerb)
		if matches {
			candidates.add(curlyRoute{eachRoute, paramCount, staticCount}) // TODO make sure Routes() return pointers?
		}
	}
	sort.Sort(candidates)
	return candidates
}

// matchesRouteByPathTokens computes whether it matches, howmany parameters do match and what the number of static path elements are.
func (c CurlyRouter) matchesRouteByPathTokens(routeTokens, requestTokens []string, routeHasCustomVerb bool) (matches bool, paramCount int, staticCount int) {
	if len(routeTokens) < len(requestTokens) {
		// proceed in matching only if last routeToken is wildcard
		count := len(routeTokens)
		if count == 0 || !strings.HasSuffix(routeTokens[count-1], "*}") {
			return false, 0, 0
		}
		// proceed
	}
	for i, routeToken := range routeTokens {
		if i == len(requestTokens) {
			// reached end of request path
			return false, 0, 0
		}
		requestToken := requestTokens[i]
		if routeHasCustomVerb && hasCustomVerb(routeToken) {
			if !isMatchCustomVerb(routeToken, requestToken) {
				return false, 0, 0
			}
			staticCount++
			requestToken = removeCustomVerb(requestToken)
			routeToken = removeCustomVerb(routeToken)
		}

		if strings.HasPrefix(routeToken, "{") {
			paramCount++
			if colon := strings.Index(routeToken, ":"); colon != -1 {
				// match by regex
				matchesToken, matchesRemainder := c.regularMatchesPathToken(routeToken, colon, requestToken)
				if !matchesToken {
					return false, 0, 0
				}
				if matchesRemainder {
					break
				}
			}
		} else { // no { prefix
			if requestToken != routeToken {
				return false, 0, 0
			}
			staticCount++
		}
	}
	return true, paramCount, staticCount
}

// regularMatchesPathToken tests whether the regular expression part of routeToken matches the requestToken or all remaining tokens
// format routeToken is {someVar:someExpression}, e.g. {zipcode:[\d][\d][\d][\d][A-Z][A-Z]}
func (c CurlyRouter) regularMatchesPathToken(routeToken string, colon int, requestToken string) (matchesToken bool, matchesRemainder bool) {
	regPart := routeToken[colon+1 : len(routeToken)-1]
	if regPart == "*" {
		if trace {
			traceLogger.Printf("wildcard parameter detected in route token %s that matches %s\n", routeToken, requestToken)
		}
		return true, true
	}
	matched, err := regexp.MatchString(regPart, requestToken)
	return (matched && err == nil), false
}

var jsr311Router = RouterJSR311{}

// detectRoute selectes from a list of Route the first match by inspecting both the Accept and Content-Type
// headers of the Request. See also RouterJSR311 in jsr311.go
func (c CurlyRouter) detectRoute(candidateRoutes sortableCurlyRoutes, httpRequest *http.Request) (*Route, error) {
	// tracing is done inside detectRoute
	return jsr311Router.detectRoute(candidateRoutes.routes(), httpRequest)
}

// detectWebService returns the best matching webService given the list of path tokens.
// see also computeWebserviceScore
func (c CurlyRouter) detectWebService(requestTokens []string, webServices []*WebService) *WebService {
	var bestWs *WebService
	score := -1
	for _, eachWS := range webServices {
		matches, eachScore := c.computeWebserviceScore(requestTokens, eachWS.pathExpr.tokens)
		if matches && (eachScore > score) {
			bestWs = eachWS
			score = eachScore
		}
	}
	return bestWs
}

// computeWebserviceScore returns whether tokens match and
// the weighted score of the longest matching consecutive tokens from the beginning.
func (c CurlyRouter) computeWebserviceScore(requestTokens []string, routeTokens []string) (bool, int) {
	if len(routeTokens) > len(requestTokens) {
		return false, 0
	}
	score := 0
	for i := 0; i < len(routeTokens); i++ {
		eachRequestToken := requestTokens[i]
		eachRouteToken := routeTokens[i]
		if len(eachRequestToken) == 0 && len(eachRouteToken) == 0 {
			score++
			continue
		}
		if len(eachRouteToken) > 0 && strings.HasPrefix(eachRouteToken, "{") {
			// no empty match
			if len(eachRequestToken) == 0 {
				return false, score
			}
			score++

			if colon := strings.Index(eachRouteToken, ":"); colon != -1 {
				// match by regex
				matchesToken, _ := c.regularMatchesPathToken(eachRouteToken, colon, eachRequestToken)
				if matchesToken {
					score++ // extra score for regex match
				}
			}			
		} else {
			// not a parameter
			if eachRequestToken != eachRouteToken {
				return false, score
			}
			score += (len(routeTokens) - i) * 10 //fuzzy
		}
	}
	return true, score
}
