/*
Package goproxy implements a minimalist Go module proxy handler.
*/
package goproxy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/mod/sumdb"
	"golang.org/x/mod/sumdb/dirhash"
	"golang.org/x/net/idna"
)

// regModuleVersionNotFound is a regular expression that used to report whether
// a message means a module version is not found.
var regModuleVersionNotFound = regexp.MustCompile(
	`(400 Bad Request)|` +
		`(403 Forbidden)|` +
		`(404 Not Found)|` +
		`(410 Gone)|` +
		`(^bad request: .*)|` +
		`(^gone: .*)|` +
		`(^not found: .*)|` +
		`(could not read Username)|` +
		`(does not contain package)|` +
		`(go.mod has non-.* module path)|` +
		`(go.mod has post-.* module path)|` +
		`(invalid .* import path)|` +
		`(invalid pseudo-version)|` +
		`(invalid version)|` +
		`(missing .*/go.mod and .*/go.mod at revision)|` +
		`(no matching versions)|` +
		`(repository .* not found)|` +
		`(unable to connect to)|` +
		`(unknown revision)|` +
		`(unrecognized import path)` +
		`(untrusted revision)|`,
)

// Goproxy is the top-level struct of this project.
//
// Note that the `Goproxy` will not mess with your environment variables, it
// will still follow your GOPROXY, GONOPROXY, GOSUMDB, GONOSUMDB, and GOPRIVATE.
// It means that you can set GOPROXY to serve the `Goproxy` itself under other
// proxies, and by setting GONOPROXY and GOPRIVATE to indicate which modules the
// `Goproxy` should download directly instead of using those proxies. And of
// course, you can also set GOSUMDB, GONOSUMDB, and GOPRIVATE to indicate how
// the `Goproxy` should verify the modules.
//
// ATTENTION: Since GONOPROXY, GOSUMDB, GONOSUMDB, and GOPRIVATE were first
// introduced in Go 1.13, so we implemented a built-in support for them. Now,
// you can set them even before Go 1.13.
//
// It is highly recommended not to modify the value of any field of the
// `Goproxy` after calling the `Goproxy.ServeHTTP`, which will cause
// unpredictable problems.
//
// The new instances of the `Goproxy` should only be created by calling the
// `New`.
type Goproxy struct {
	// GoBinName is the name of the Go binary.
	//
	// Default value: "go"
	GoBinName string `mapstructure:"go_bin_name"`

	// GoBinEnv is the environment of the Go binary. Each entry is of the
	// form "key=value".
	//
	// If the `GoBinEnv` contains duplicate environment keys, only the last
	// value in the slice for each duplicate key is used.
	//
	// Default value: `os.Environ()`
	GoBinEnv []string `mapstructure:"go_bin_env"`

	// MaxGoBinWorkers is the maximum number of the Go binary commands that
	// are allowed to execute at the same time.
	//
	// If the `MaxGoBinWorkers` is zero, then there will be no limitations.
	//
	// Default value: 0
	MaxGoBinWorkers int `mapstructure:"max_go_bin_workers"`

	// PathPrefix is the prefix of all request paths. It will be used to
	// trim the request paths via `strings.TrimPrefix`.
	//
	// Note that when the `PathPrefix` is not empty, then it should start
	// with "/".
	//
	// Default value: ""
	PathPrefix string `mapstructure:"path_prefix"`

	// Cacher is the `Cacher` that used to cache module files.
	//
	// If the `Cacher` is nil, the module files will be temporarily stored
	// in the local disk and discarded as the request ends.
	//
	// Default value: nil
	Cacher Cacher `mapstructure:"cacher"`

	// MaxZIPCacheBytes is the maximum number of bytes of the ZIP cache that
	// will be stored in the `Cacher`.
	//
	// If the `MaxZIPCacheBytes` is zero, then there will be no limitations.
	//
	// Default value: 0
	MaxZIPCacheBytes int `mapstructure:"max_zip_cache_bytes"`

	// SupportedSUMDBNames is the supported checksum database names.
	//
	// Default value: ["sum.golang.org"]
	SupportedSUMDBNames []string `mapstructure:"supported_sumdb_names"`

	// ErrorLogger is the `log.Logger` that logs errors that occur while
	// proxing.
	//
	// If the `ErrorLogger` is nil, logging is done via the "log" package's
	// standard logger.
	//
	// Default value: nil
	ErrorLogger *log.Logger `mapstructure:"-"`

	// DisableNotFoundLog is a switch that disables "Not Found" log.
	//
	// Default value: false
	DisableNotFoundLog bool `mapstructure:"disable_not_found_log"`

	loadOnce            *sync.Once
	goBinEnv            map[string]string
	goBinWorkerChan     chan struct{}
	sumdbClient         *sumdb.Client
	supportedSUMDBNames map[string]bool
}

// New returns a new instance of the `Goproxy` with default field values.
//
// The `New` is the only function that creates new instances of the `Goproxy`
// and keeps everything working.
func New() *Goproxy {
	return &Goproxy{
		GoBinName:           "go",
		GoBinEnv:            os.Environ(),
		SupportedSUMDBNames: []string{"sum.golang.org"},
		loadOnce:            &sync.Once{},
		goBinEnv:            map[string]string{},
		supportedSUMDBNames: map[string]bool{},
	}
}

// load loads the stuff of the g up.
func (g *Goproxy) load() {
	for _, env := range g.GoBinEnv {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) != 2 {
			continue
		}

		g.goBinEnv[parts[0]] = parts[1]
	}

	if g.MaxGoBinWorkers != 0 {
		g.goBinWorkerChan = make(chan struct{}, g.MaxGoBinWorkers)
	}

	var proxies []string
	for _, proxy := range strings.Split(g.goBinEnv["GOPROXY"], ",") {
		proxy = strings.TrimSpace(proxy)
		if proxy == "" {
			continue
		}

		proxies = append(proxies, proxy)
		if proxy == "direct" || proxy == "off" {
			break
		}
	}

	if len(proxies) > 0 {
		g.goBinEnv["GOPROXY"] = strings.Join(proxies, ",")
	} else if g.goBinEnv["GOPROXY"] == "" {
		g.goBinEnv["GOPROXY"] = "https://proxy.golang.org,direct"
	} else {
		g.goBinEnv["GOPROXY"] = "off"
	}

	g.goBinEnv["GOSUMDB"] = strings.TrimSpace(g.goBinEnv["GOSUMDB"])
	switch g.goBinEnv["GOSUMDB"] {
	case "", "sum.golang.org":
		g.goBinEnv["GOSUMDB"] = "sum.golang.org" +
			"+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8"
	}

	if g.goBinEnv["GONOPROXY"] == "" {
		g.goBinEnv["GONOPROXY"] = g.goBinEnv["GOPRIVATE"]
	}

	var noproxies []string
	for _, noproxy := range strings.Split(g.goBinEnv["GONOPROXY"], ",") {
		noproxy = strings.TrimSpace(noproxy)
		if noproxy == "" {
			continue
		}

		noproxies = append(noproxies, noproxy)
	}

	if len(noproxies) > 0 {
		g.goBinEnv["GONOPROXY"] = strings.Join(noproxies, ",")
	}

	if g.goBinEnv["GONOSUMDB"] == "" {
		g.goBinEnv["GONOSUMDB"] = g.goBinEnv["GOPRIVATE"]
	}

	var nosumdbs []string
	for _, nosumdb := range strings.Split(g.goBinEnv["GONOSUMDB"], ",") {
		nosumdb = strings.TrimSpace(nosumdb)
		if nosumdb == "" {
			continue
		}

		nosumdbs = append(nosumdbs, nosumdb)
	}

	if len(nosumdbs) > 0 {
		g.goBinEnv["GONOSUMDB"] = strings.Join(nosumdbs, ",")
	}

	g.sumdbClient = sumdb.NewClient(&sumdbClientOps{
		envGOPROXY:  g.goBinEnv["GOPROXY"],
		envGOSUMDB:  g.goBinEnv["GOSUMDB"],
		errorLogger: g.ErrorLogger,
	})

	for _, name := range g.SupportedSUMDBNames {
		if n, err := idna.Lookup.ToASCII(name); err == nil {
			g.supportedSUMDBNames[n] = true
		}
	}
}

var stagingRepos = []string{
	"k8s.io/api",
	"k8s.io/apiextensions-apiserver",
	"k8s.io/apimachinery",
	"k8s.io/apiserver",
	"k8s.io/cli-runtime",
	"k8s.io/client-go",
	"k8s.io/cloud-provider",
	"k8s.io/cluster-bootstrap",
	"k8s.io/code-generator",
	"k8s.io/component-base",
	"k8s.io/cri-api",
	"k8s.io/csi-translation-lib",
	"k8s.io/kube-aggregator",
	"k8s.io/kube-controller-manager",
	"k8s.io/kube-proxy",
	"k8s.io/kube-scheduler",
	"k8s.io/kubectl",
	"k8s.io/kubelet",
	"k8s.io/legacy-cloud-providers",
	"k8s.io/metrics",
	"k8s.io/node-api",
	"k8s.io/sample-apiserver",
	"k8s.io/sample-cli-plugin",
	"k8s.io/sample-controller",
}

// ServeHTTP implements the `http.Handler`.
func (g *Goproxy) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	g.loadOnce.Do(g.load)

	switch r.Method {
	case http.MethodGet, http.MethodHead:
	default:
		setResponseCacheControlHeader(rw, 3600)
		responseMethodNotAllowed(rw)
		return
	}

	if !strings.HasPrefix(r.URL.Path, "/") {
		setResponseCacheControlHeader(rw, 3600)
		responseNotFound(rw)
		return
	}

	trimmedPath := path.Clean(r.URL.Path)
	trimmedPath = strings.TrimPrefix(trimmedPath, g.PathPrefix)
	trimmedPath = strings.TrimLeft(trimmedPath, "/")

	name, err := url.PathUnescape(trimmedPath)
	if err != nil {
		setResponseCacheControlHeader(rw, 3600)
		responseNotFound(rw)
		return
	}

	cachingForever := false
	if strings.HasPrefix(name, "sumdb/") {
		sumdbURL, err := parseRawURL(strings.TrimPrefix(name, "sumdb/"))
		if err != nil {
			setResponseCacheControlHeader(rw, 3600)
			responseNotFound(rw)
			return
		}

		sumdbName, err := idna.Lookup.ToASCII(sumdbURL.Host)
		if err != nil {
			setResponseCacheControlHeader(rw, 3600)
			responseNotFound(rw)
			return
		}

		if !g.supportedSUMDBNames[sumdbName] {
			setResponseCacheControlHeader(rw, 60)
			responseNotFound(rw)
			return
		}

		var contentType string
		switch {
		case sumdbURL.Path == "/supported":
			setResponseCacheControlHeader(rw, 60)
			rw.Write(nil) // 200 OK
			return
		case sumdbURL.Path == "/latest":
			contentType = "text/plain; charset=utf-8"
		case strings.HasPrefix(sumdbURL.Path, "/lookup/"):
			cachingForever = true
			contentType = "text/plain; charset=utf-8"
		case strings.HasPrefix(sumdbURL.Path, "/tile/"):
			cachingForever = true
			contentType = "application/octet-stream"
		default:
			setResponseCacheControlHeader(rw, 3600)
			responseNotFound(rw)
			return
		}

		sumdbReq, err := http.NewRequest(
			http.MethodGet,
			sumdbURL.String(),
			nil,
		)
		if err != nil {
			g.logError(err)
			responseInternalServerError(rw)
			return
		}

		sumdbReq = sumdbReq.WithContext(r.Context())

		sumdbRes, err := http.DefaultClient.Do(sumdbReq)
		if err != nil {
			if ue, ok := err.(*url.Error); ok && ue.Timeout() {
				responseBadGateway(rw)
			} else {
				g.logError(err)
				responseInternalServerError(rw)
			}

			return
		}
		defer sumdbRes.Body.Close()

		if sumdbRes.StatusCode != http.StatusOK {
			b, err := ioutil.ReadAll(sumdbRes.Body)
			if err != nil {
				g.logError(err)
				responseInternalServerError(rw)
				return
			}

			switch sumdbRes.StatusCode {
			case http.StatusBadRequest,
				http.StatusNotFound,
				http.StatusGone:
				if !g.DisableNotFoundLog {
					g.logErrorf("%s", b)
				}

				if sumdbRes.StatusCode == http.StatusNotFound {
					setResponseCacheControlHeader(rw, 60)
				} else {
					setResponseCacheControlHeader(rw, 3600)
				}

				responseNotFound(rw, string(b))

				return
			}

			g.logError(fmt.Errorf(
				"GET %s: %s: %s",
				redactedURL(sumdbURL),
				sumdbRes.Status,
				b,
			))
			responseBadGateway(rw)

			return
		}

		rw.Header().Set("Content-Type", contentType)
		rw.Header().Set(
			"Content-Length",
			sumdbRes.Header.Get("Content-Length"),
		)

		if cachingForever {
			setResponseCacheControlHeader(rw, 365*24*3600)
		} else {
			setResponseCacheControlHeader(rw, 60)
		}

		io.Copy(rw, sumdbRes.Body)

		return
	}

	isLatest := false
	isList := false
	switch {
	case strings.HasSuffix(name, "/@latest"):
		name = fmt.Sprint(
			strings.TrimSuffix(name, "latest"),
			"v/latest.info",
		)
		isLatest = true
	case strings.HasSuffix(name, "/@v/list"):
		name = fmt.Sprint(
			strings.TrimSuffix(name, "list"),
			"latest.info",
		)
		isList = true
	}

	nameParts := strings.Split(name, "/@v/")
	if len(nameParts) != 2 {
		setResponseCacheControlHeader(rw, 3600)
		responseNotFound(rw)
		return
	}

	escapedModulePath := nameParts[0]
	modulePath, err := module.UnescapePath(escapedModulePath)
	if err != nil {
		setResponseCacheControlHeader(rw, 3600)
		responseNotFound(rw)
		return
	}

	nameBase := nameParts[1]
	nameExt := path.Ext(nameBase)
	switch nameExt {
	case ".info", ".mod", ".zip":
	default:
		setResponseCacheControlHeader(rw, 3600)
		responseNotFound(rw)
		return
	}

	escapedModuleVersion := strings.TrimSuffix(nameBase, nameExt)
	moduleVersion, err := module.UnescapeVersion(escapedModuleVersion)
	if err != nil {
		setResponseCacheControlHeader(rw, 3600)
		responseNotFound(rw)
		return
	}

	isStagingRepo := false
	for _, sr := range stagingRepos {
		if sr == modulePath {
			fmt.Println("isStagingRepo", modulePath)
			isStagingRepo = true
			break
		}
	}
	if isStagingRepo && strings.HasPrefix(moduleVersion, "v0.1") {
		oldModuleVersion := moduleVersion
		moduleVersion = "kubernetes-1." + strings.TrimPrefix(moduleVersion, "v0.")
		fmt.Println(oldModuleVersion, "->", moduleVersion)
	}

	goproxyRoot, err := ioutil.TempDir("", "goproxy")
	if err != nil {
		g.logError(err)
		responseInternalServerError(rw)
		return
	}

	hijackedGoproxyRootPurge := false
	defer func() {
		if !hijackedGoproxyRootPurge {
			modClean(g.GoBinName, g.goBinEnv, goproxyRoot)
			os.RemoveAll(goproxyRoot)
		}
	}()

	if isList {
		mr, err := mod(
			"list",
			g.GoBinName,
			g.goBinEnv,
			g.goBinWorkerChan,
			goproxyRoot,
			modulePath,
			moduleVersion,
		)
		if err != nil {
			if regModuleVersionNotFound.MatchString(err.Error()) {
				if !g.DisableNotFoundLog {
					g.logError(err)
				}

				setResponseCacheControlHeader(rw, 60)
				responseNotFound(rw, err)
			} else {
				g.logError(err)
				responseInternalServerError(rw)
			}

			return
		}

		if isStagingRepo {
			fmt.Printf("versions %v\n", mr.Versions)
			for _, v := range mr.Versions {
				if strings.HasPrefix(v, "kubernetes-1.") {
					mrCopy := *mr
					zeroVer := "v0." + strings.TrimPrefix(moduleVersion, "kubernetes-1.")
					mrCopy.Versions = append(mrCopy.Versions, zeroVer)
					mr = &mrCopy

					fmt.Println("adding", zeroVer, "to the list")

					break
				}
			}
		}

		versions := strings.Join(mr.Versions, "\n")

		setResponseCacheControlHeader(rw, 60)
		responseString(rw, http.StatusOK, versions)

		return
	} else if isLatest || !semver.IsValid(moduleVersion) {
		var operation string
		if isLatest {
			operation = "latest"
		} else {
			operation = "lookup"
		}

		mr, err := mod(
			operation,
			g.GoBinName,
			g.goBinEnv,
			g.goBinWorkerChan,
			goproxyRoot,
			modulePath,
			moduleVersion,
		)
		if err != nil {
			if regModuleVersionNotFound.MatchString(err.Error()) {
				if !g.DisableNotFoundLog {
					g.logError(err)
				}

				setResponseCacheControlHeader(rw, 60)
				responseNotFound(rw, err)
			} else {
				g.logError(err)
				responseInternalServerError(rw)
			}

			return
		}

		moduleVersion = mr.Version
		escapedModuleVersion, err = module.EscapeVersion(moduleVersion)
		if err != nil {
			g.logError(err)
			responseInternalServerError(rw)
			return
		}

		nameBase = fmt.Sprint(escapedModuleVersion, nameExt)
		name = path.Join(path.Dir(name), nameBase)
	} else {
		cachingForever = true
	}

	cacher := g.Cacher
	if cacher == nil {
		cacher = &tempCacher{}
	}

	cache, err := cacher.Cache(r.Context(), name)
	if err == ErrCacheNotFound {
		mr, err := mod(
			"download",
			g.GoBinName,
			g.goBinEnv,
			g.goBinWorkerChan,
			goproxyRoot,
			modulePath,
			moduleVersion,
		)
		if err != nil {
			if regModuleVersionNotFound.MatchString(err.Error()) {
				if !g.DisableNotFoundLog {
					g.logError(err)
				}

				setResponseCacheControlHeader(rw, 60)
				responseNotFound(rw, err)
			} else {
				g.logError(err)
				responseInternalServerError(rw)
			}

			return
		}

		if g.goBinEnv["GOSUMDB"] != "off" &&
			!globsMatchPath(g.goBinEnv["GONOSUMDB"], modulePath) {
			zipLines, err := g.sumdbClient.Lookup(
				modulePath,
				moduleVersion,
			)
			if err != nil {
				err := errors.New(strings.TrimPrefix(
					err.Error(),
					fmt.Sprintf(
						"%s@%s: ",
						modulePath,
						moduleVersion,
					),
				))

				if regModuleVersionNotFound.MatchString(
					err.Error(),
				) {
					if !g.DisableNotFoundLog {
						g.logError(err)
					}

					setResponseCacheControlHeader(rw, 60)
					responseNotFound(rw, err)
				} else {
					g.logError(err)
					responseInternalServerError(rw)
				}

				return
			}

			zipHash, err := dirhash.HashZip(
				mr.Zip,
				dirhash.DefaultHash,
			)
			if err != nil {
				g.logError(err)
				responseInternalServerError(rw)
				return
			}

			if !stringSliceContains(
				zipLines,
				fmt.Sprintf(
					"%s %s %s",
					modulePath,
					moduleVersion,
					zipHash,
				),
			) {
				setResponseCacheControlHeader(rw, 3600)
				responseNotFound(rw, fmt.Sprintf(
					"untrusted revision %s",
					moduleVersion,
				))
				return
			}

			goModLines, err := g.sumdbClient.Lookup(
				modulePath,
				fmt.Sprint(moduleVersion, "/go.mod"),
			)
			if err != nil {
				err := errors.New(strings.TrimPrefix(
					err.Error(),
					fmt.Sprintf(
						"%s@%s: ",
						modulePath,
						moduleVersion,
					),
				))

				if regModuleVersionNotFound.MatchString(
					err.Error(),
				) {
					if !g.DisableNotFoundLog {
						g.logError(err)
					}

					setResponseCacheControlHeader(rw, 60)
					responseNotFound(rw, err)
				} else {
					g.logError(err)
					responseInternalServerError(rw)
				}

				return
			}

			goModHash, err := dirhash.Hash1(
				[]string{"go.mod"},
				func(string) (io.ReadCloser, error) {
					return os.Open(mr.GoMod)
				},
			)
			if err != nil {
				g.logError(err)
				responseInternalServerError(rw)
				return
			}

			if !stringSliceContains(
				goModLines,
				fmt.Sprintf(
					"%s %s/go.mod %s",
					modulePath,
					moduleVersion,
					goModHash,
				),
			) {
				setResponseCacheControlHeader(rw, 3600)
				responseNotFound(rw, fmt.Sprintf(
					"untrusted revision %s",
					moduleVersion,
				))
				return
			}
		}

		// Setting the caches asynchronously to avoid timeouts in
		// response.
		hijackedGoproxyRootPurge = true
		go func() {
			defer func() {
				modClean(g.GoBinName, g.goBinEnv, goproxyRoot)
				os.RemoveAll(goproxyRoot)
			}()

			namePrefix := strings.TrimSuffix(name, nameExt)

			// Using a new `context.Context` instead of the
			// `r.Context` to avoid early timeouts.
			ctx, cancel := context.WithTimeout(
				context.Background(),
				10*time.Minute,
			)
			defer cancel()

			infoCache, err := newTempCache(
				mr.Info,
				fmt.Sprint(namePrefix, ".info"),
				cacher.NewHash(),
			)
			if err != nil {
				g.logError(err)
				return
			}
			defer infoCache.Close()

			if err := cacher.SetCache(ctx, infoCache); err != nil {
				g.logError(err)
				return
			}

			modCache, err := newTempCache(
				mr.GoMod,
				fmt.Sprint(namePrefix, ".mod"),
				cacher.NewHash(),
			)
			if err != nil {
				g.logError(err)
				return
			}
			defer modCache.Close()

			if err := cacher.SetCache(ctx, modCache); err != nil {
				g.logError(err)
				return
			}

			zipCache, err := newTempCache(
				mr.Zip,
				fmt.Sprint(namePrefix, ".zip"),
				cacher.NewHash(),
			)
			if err != nil {
				g.logError(err)
				return
			}
			defer zipCache.Close()

			if g.MaxZIPCacheBytes == 0 ||
				zipCache.Size() <= int64(g.MaxZIPCacheBytes) {
				err := cacher.SetCache(ctx, zipCache)
				if err != nil {
					g.logError(err)
					return
				}
			}
		}()

		var filename string
		switch nameExt {
		case ".info":
			filename = mr.Info
		case ".mod":
			filename = mr.GoMod
		case ".zip":
			filename = mr.Zip
		}

		cache, err = newTempCache(filename, name, cacher.NewHash())
		if err != nil {
			g.logError(err)
			responseInternalServerError(rw)
			return
		}
	} else if err != nil {
		g.logError(err)
		responseInternalServerError(rw)
		return
	}
	defer cache.Close()

	rw.Header().Set("Content-Type", cache.MIMEType())
	rw.Header().Set(
		"ETag",
		fmt.Sprintf(
			"%q",
			base64.StdEncoding.EncodeToString(cache.Checksum()),
		),
	)

	if cachingForever {
		setResponseCacheControlHeader(rw, 365*24*3600)
	} else {
		setResponseCacheControlHeader(rw, 60)
	}

	http.ServeContent(rw, r, "", cache.ModTime(), cache)
}

// logErrorf logs the v as an error in the format.
func (g *Goproxy) logErrorf(format string, v ...interface{}) {
	s := fmt.Sprintf(format, v...)
	if g.ErrorLogger != nil {
		g.ErrorLogger.Output(2, s)
	} else {
		log.Output(2, s)
	}
}

// logError logs the err.
func (g *Goproxy) logError(err error) {
	g.logErrorf("%v", err)
}

// parseRawURL parses the rawURL.
func parseRawURL(rawURL string) (*url.URL, error) {
	if strings.ContainsAny(rawURL, ".:/") &&
		!strings.Contains(rawURL, ":/") &&
		!filepath.IsAbs(rawURL) &&
		!path.IsAbs(rawURL) {
		rawURL = fmt.Sprint("https://", rawURL)
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return nil, fmt.Errorf(
			"invalid URL scheme (must be http or https): %s",
			redactedURL(u),
		)
	}

	return u, nil
}

// appendURL appends the extraPaths to the u safely and reutrns a new instance
// of the `url.URL`.
func appendURL(u *url.URL, extraPaths ...string) *url.URL {
	nu := *u
	u = &nu
	for _, ep := range extraPaths {
		u.Path = path.Join(u.Path, ep)
		u.RawPath = path.Join(
			u.RawPath,
			strings.Replace(url.PathEscape(ep), "%2F", "/", -1),
		)
	}

	return u
}

// redactedURL returns a redacted string form of the u, suitable for printing in
// error messages. The string form replaces any non-empty password in the u with
// "[redacted]".
func redactedURL(u *url.URL) string {
	if u.User != nil {
		if _, ok := u.User.Password(); ok {
			nu := *u
			u = &nu
			u.User = url.UserPassword(
				u.User.Username(),
				"[redacted]",
			)
		}
	}

	return u.String()
}

// stringSliceContains reports whether the ss contains the s.
func stringSliceContains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}

	return false
}

// globsMatchPath reports whether any path prefix of target matches one of the
// glob patterns (as defined by the `path.Match`) in the comma-separated globs
// list. It ignores any empty or malformed patterns in the list.
func globsMatchPath(globs, target string) bool {
	for globs != "" {
		// Extract next non-empty glob in comma-separated list.
		var glob string
		if i := strings.Index(globs, ","); i >= 0 {
			glob, globs = globs[:i], globs[i+1:]
		} else {
			glob, globs = globs, ""
		}

		if glob == "" {
			continue
		}

		// A glob with N+1 path elements (N slashes) needs to be matched
		// against the first N+1 path elements of target, which end just
		// before the N+1'th slash.
		n := strings.Count(glob, "/")
		prefix := target

		// Walk target, counting slashes, truncating at the N+1'th
		// slash.
		for i := 0; i < len(target); i++ {
			if target[i] == '/' {
				if n == 0 {
					prefix = target[:i]
					break
				}

				n--
			}
		}

		if n > 0 {
			// Not enough prefix elements.
			continue
		}

		if matched, _ := path.Match(glob, prefix); matched {
			return true
		}
	}

	return false
}
