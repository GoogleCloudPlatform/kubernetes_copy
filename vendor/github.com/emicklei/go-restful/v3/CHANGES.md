# Change history of go-restful


## [v3.12.1] - 2024-05-28

-  fix misroute when dealing multiple webservice with regex (#549) (thanks Haitao Chen)

## [v3.12.0] - 2024-03-11

- add Flush method #529 (#538)
- fix: Improper handling of empty POST requests (#543)

## [v3.11.3] - 2024-01-09

- better not have 2 tags on one commit

## [v3.11.1, v3.11.2] - 2024-01-09

- fix by restoring custom JSON handler functions (Mike Beaumont #540)

## [v3.12.0] - 2023-08-19

- restored behavior as <= v3.9.0 with option to change path strategy using TrimRightSlashEnabled. 

## [v3.10.2] - 2023-03-09 - DO NOT USE

- introduced MergePathStrategy to be able to revert behaviour of path concatenation to 3.9.0
  see comment in Readme how to customize this behaviour.

## [v3.10.1] - 2022-11-19 - DO NOT USE

- fix broken 3.10.0 by using path package for joining paths

## [v3.10.0] - 2022-10-11 - BROKEN

- changed tokenizer to match std route match behavior; do not trimright the path (#511)
- Add MIME_ZIP (#512)
- Add MIME_ZIP and HEADER_ContentDisposition (#513)
- Changed how to get query parameter issue #510

## [v3.9.0] - 2022-07-21

- add support for http.Handler implementations to work as FilterFunction, issue #504 (thanks to https://github.com/ggicci)

## [v3.8.0] - 2022-06-06

- use exact matching of allowed domain entries, issue #489 (#493)
	- this changes fixes [security] Authorization Bypass Through User-Controlled Key
	  by changing the behaviour of the AllowedDomains setting in the CORS filter.
	  To support the previous behaviour, the CORS filter type now has a AllowedDomainFunc
	  callback mechanism which is called when a simple domain match fails. 
- add test and fix for POST without body and Content-type, issue #492 (#496)
- [Minor] Bad practice to have a mix of Receiver types. (#491)

## [v3.7.2] - 2021-11-24

- restored FilterChain (#482 by SVilgelm)


## [v3.7.1] - 2021-10-04

- fix problem with contentEncodingEnabled setting (#479)

## [v3.7.0] - 2021-09-24

- feat(parameter): adds additional openapi mappings (#478)

## [v3.6.0] - 2021-09-18

- add support for vendor extensions (#477 thx erraggy)

## [v3.5.2] - 2021-07-14

- fix removing absent route from webservice (#472)

## [v3.5.1] - 2021-04-12

- fix handling no match access selected path
- remove obsolete field

## [v3.5.0] - 2021-04-10

- add check for wildcard (#463) in CORS
- add access to Route from Request, issue #459 (#462)

## [v3.4.0] - 2020-11-10

- Added OPTIONS to WebService

## [v3.3.2] - 2020-01-23

- Fixed duplicate compression in dispatch. #449


## [v3.3.1] - 2020-08-31

- Added check on writer to prevent compression of response twice. #447

## [v3.3.0] - 2020-08-19

- Enable content encoding on Handle and ServeHTTP (#446)
- List available representations in 406 body (#437)
- Convert to string using rune() (#443)

## [v3.2.0] - 2020-06-21

- 405 Method Not Allowed must have Allow header (#436) (thx Bracken <abdawson@gmail.com>)
- add field allowedMethodsWithoutContentType (#424)

## [v3.1.0]

- support describing response headers (#426)
- fix openapi examples (#425)

v3.0.0

- fix: use request/response resulting from filter chain
- add Go module
  Module consumer should use github.com/emicklei/go-restful/v3 as import path

v2.10.0

- support for Custom Verbs (thanks Vinci Xu <277040271@qq.com>)
- fixed static example (thanks Arthur <yang_yapo@126.com>)
- simplify code (thanks Christian Muehlhaeuser <muesli@gmail.com>)
- added JWT HMAC with SHA-512 authentication code example (thanks Amim Knabben <amim.knabben@gmail.com>)

v2.9.6

- small optimization in filter code

v2.11.1

- fix WriteError return value (#415)

v2.11.0 

- allow prefix and suffix in path variable expression (#414)

v2.9.6

- support google custome verb (#413)

v2.9.5

- fix panic in Response.WriteError if err == nil

v2.9.4

- fix issue #400 , parsing mime type quality
- Route Builder added option for contentEncodingEnabled (#398)

v2.9.3

- Avoid return of 415 Unsupported Media Type when request body is empty (#396)

v2.9.2

- Reduce allocations in per-request methods to improve performance (#395)

v2.9.1

- Fix issue with default responses and invalid status code 0. (#393)

v2.9.0

- add per Route content encoding setting (overrides container setting)

v2.8.0

- add Request.QueryParameters()
- add json-iterator (via build tag)
- disable vgo module (until log is moved)

v2.7.1

- add vgo module

v2.6.1

- add JSONNewDecoderFunc to allow custom JSON Decoder usage (go 1.10+)

v2.6.0

- Make JSR 311 routing and path param processing consistent
- Adding description to RouteBuilder.Reads()
- Update example for Swagger12 and OpenAPI

2017-09-13

- added route condition functions using `.If(func)` in route building.

2017-02-16

- solved issue #304, make operation names unique

2017-01-30
 
	[IMPORTANT] For swagger users, change your import statement to:	
	swagger "github.com/emicklei/go-restful-swagger12"

- moved swagger 1.2 code to go-restful-swagger12
- created TAG 2.0.0

2017-01-27

- remove defer request body close
- expose Dispatch for testing filters and Routefunctions
- swagger response model cannot be array 
- created TAG 1.0.0

2016-12-22

- (API change) Remove code related to caching request content. Removes SetCacheReadEntity(doCache bool)

2016-11-26

- Default change! now use CurlyRouter (was RouterJSR311)
- Default change! no more caching of request content
- Default change! do not recover from panics

2016-09-22

- fix the DefaultRequestContentType feature

2016-02-14

- take the qualify factor of the Accept header mediatype into account when deciding the contentype of the response
- add constructors for custom entity accessors for xml and json 

2015-09-27

- rename new WriteStatusAnd... to WriteHeaderAnd... for consistency

2015-09-25

- fixed problem with changing Header after WriteHeader (issue 235)

2015-09-14

- changed behavior of WriteHeader (immediate write) and WriteEntity (no status write)
- added support for custom EntityReaderWriters.

2015-08-06

- add support for reading entities from compressed request content
- use sync.Pool for compressors of http response and request body
- add Description to Parameter for documentation in Swagger UI

2015-03-20

- add configurable logging

2015-03-18

- if not specified, the Operation is derived from the Route function

2015-03-17

- expose Parameter creation functions
- make trace logger an interface
- fix OPTIONSFilter
- customize rendering of ServiceError
- JSR311 router now handles wildcards
- add Notes to Route

2014-11-27

- (api add) PrettyPrint per response. (as proposed in #167)

2014-11-12

- (api add) ApiVersion(.) for documentation in Swagger UI

2014-11-10

- (api change) struct fields tagged with "description" show up in Swagger UI

2014-10-31

- (api change) ReturnsError -> Returns
- (api add)    RouteBuilder.Do(aBuilder) for DRY use of RouteBuilder
- fix swagger nested structs
- sort Swagger response messages by code

2014-10-23

- (api add) ReturnsError allows you to document Http codes in swagger
- fixed problem with greedy CurlyRouter
- (api add) Access-Control-Max-Age in CORS
- add tracing functionality (injectable) for debugging purposes
- support JSON parse 64bit int 
- fix empty parameters for swagger
- WebServicesUrl is now optional for swagger
- fixed duplicate AccessControlAllowOrigin in CORS
- (api change) expose ServeMux in container
- (api add) added AllowedDomains in CORS
- (api add) ParameterNamed for detailed documentation

2014-04-16

- (api add) expose constructor of Request for testing.

2014-06-27

- (api add) ParameterNamed gives access to a Parameter definition and its data (for further specification).
- (api add) SetCacheReadEntity allow scontrol over whether or not the request body is being cached (default true for compatibility reasons).

2014-07-03

- (api add) CORS can be configured with a list of allowed domains

2014-03-12

- (api add) Route path parameters can use wildcard or regular expressions. (requires CurlyRouter)

2014-02-26

- (api add) Request now provides information about the matched Route, see method SelectedRoutePath 

2014-02-17

- (api change) renamed parameter constants (go-lint checks)

2014-01-10

- (api add) support for CloseNotify, see http://golang.org/pkg/net/http/#CloseNotifier

2014-01-07

- (api change) Write* methods in Response now return the error or nil.
- added example of serving HTML from a Go template.
- fixed comparing Allowed headers in CORS (is now case-insensitive)

2013-11-13

- (api add) Response knows how many bytes are written to the response body.

2013-10-29

- (api add) RecoverHandler(handler RecoverHandleFunction) to change how panic recovery is handled. Default behavior is to log and return a stacktrace. This may be a security issue as it exposes sourcecode information.

2013-10-04

- (api add) Response knows what HTTP status has been written
- (api add) Request can have attributes (map of string->interface, also called request-scoped variables

2013-09-12

- (api change) Router interface simplified
- Implemented CurlyRouter, a Router that does not use|allow regular expressions in paths

2013-08-05
 - add OPTIONS support
 - add CORS support

2013-08-27

- fixed some reported issues (see github)
- (api change) deprecated use of WriteError; use WriteErrorString instead

2014-04-15

- (fix) v1.0.1 tag: fix Issue 111: WriteErrorString

2013-08-08

- (api add) Added implementation Container: a WebServices collection with its own http.ServeMux allowing multiple endpoints per program. Existing uses of go-restful will register their services to the DefaultContainer.
- (api add) the swagger package has be extended to have a UI per container.
- if panic is detected then a small stack trace is printed (thanks to runner-mei)
- (api add) WriteErrorString to Response

Important API changes:

- (api remove) package variable DoNotRecover no longer works ; use restful.DefaultContainer.DoNotRecover(true) instead.
- (api remove) package variable EnableContentEncoding no longer works ; use restful.DefaultContainer.EnableContentEncoding(true) instead.
 
 
2013-07-06

- (api add) Added support for response encoding (gzip and deflate(zlib)). This feature is disabled on default (for backwards compatibility). Use restful.EnableContentEncoding = true in your initialization to enable this feature.

2013-06-19

- (improve) DoNotRecover option, moved request body closer, improved ReadEntity

2013-06-03

- (api change) removed Dispatcher interface, hide PathExpression
- changed receiver names of type functions to be more idiomatic Go

2013-06-02

- (optimize) Cache the RegExp compilation of Paths.

2013-05-22
	
- (api add) Added support for request/response filter functions

2013-05-18


- (api add) Added feature to change the default Http Request Dispatch function (travis cline)
- (api change) Moved Swagger Webservice to swagger package (see example restful-user)

[2012-11-14 .. 2013-05-18>
 
- See https://github.com/emicklei/go-restful/commits

2012-11-14

- Initial commit


