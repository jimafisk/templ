package templ

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/a-h/templ/safehtml"
)

// Types exposed by all components.

// Component is the interface that all templates implement.
type Component interface {
	// Render the template.
	Render(ctx context.Context, w io.Writer) error
}

// ComponentFunc converts a function that matches the Component interface's
// Render method into a Component.
type ComponentFunc func(ctx context.Context, w io.Writer) error

// Render the template.
func (cf ComponentFunc) Render(ctx context.Context, w io.Writer) error {
	return cf(ctx, w)
}

type childrenContextKey string

const contextKeyChildren = childrenContextKey("children")

func WithChildren(ctx context.Context, children Component) context.Context {
	return context.WithValue(ctx, contextKeyChildren, &children)
}

func ClearChildren(ctx context.Context) context.Context {
	return context.WithValue(ctx, contextKeyChildren, nil)
}

// NopComponent is a component that doesn't render anything.
var NopComponent = ComponentFunc(func(ctx context.Context, w io.Writer) error { return nil })

// GetChildren from the context.
func GetChildren(ctx context.Context) Component {
	component, ok := ctx.Value(contextKeyChildren).(*Component)
	if !ok || component == nil {
		return NopComponent
	}
	return *component
}

// ComponentHandler is a http.Handler that renders components.
type ComponentHandler struct {
	Component    Component
	Status       int
	ContentType  string
	ErrorHandler func(r *http.Request, err error) http.Handler
}

const componentHandlerErrorMessage = "templ: failed to render template"

// ServeHTTP implements the http.Handler interface.
func (ch ComponentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if ch.Status != 0 {
		w.WriteHeader(ch.Status)
	}
	w.Header().Add("Content-Type", ch.ContentType)
	err := ch.Component.Render(r.Context(), w)
	if err != nil {
		if ch.ErrorHandler != nil {
			ch.ErrorHandler(r, err).ServeHTTP(w, r)
			return
		}
		http.Error(w, componentHandlerErrorMessage, http.StatusInternalServerError)
	}
}

// Handler creates a http.Handler that renders the template.
func Handler(c Component, options ...func(*ComponentHandler)) *ComponentHandler {
	ch := &ComponentHandler{
		Component:   c,
		ContentType: "text/html",
	}
	for _, o := range options {
		o(ch)
	}
	return ch
}

// WithStatus sets the HTTP status code returned by the ComponentHandler.
func WithStatus(status int) func(*ComponentHandler) {
	return func(ch *ComponentHandler) {
		ch.Status = status
	}
}

// WithConentType sets the Content-Type header returned by the ComponentHandler.
func WithContentType(contentType string) func(*ComponentHandler) {
	return func(ch *ComponentHandler) {
		ch.ContentType = contentType
	}
}

// WithErrorHandler sets the error handler used if rendering fails.
func WithErrorHandler(eh func(r *http.Request, err error) http.Handler) func(*ComponentHandler) {
	return func(ch *ComponentHandler) {
		ch.ErrorHandler = eh
	}
}

// EscapeString escapes HTML text within templates.
func EscapeString(s string) string {
	return html.EscapeString(s)
}

// Bool attribute value.
func Bool(value bool) bool {
	return value
}

// Classes for CSS.
func Classes(classes ...CSSClass) CSSClasses {
	return CSSClasses(classes)
}

// CSSClasses is a slice of CSS classes.
type CSSClasses []CSSClass

// String returns the names of all CSS classes.
func (classes CSSClasses) String() string {
	if len(classes) == 0 {
		return ""
	}
	sb := new(strings.Builder)
	for i := 0; i < len(classes); i++ {
		c := classes[i]
		sb.WriteString(c.ClassName())
		if i < len(classes)-1 {
			sb.WriteRune(' ')
		}
	}
	return sb.String()
}

var safeClassName = regexp.MustCompile(`^-?[_a-zA-Z]+[_-a-zA-Z0-9]*$`)

const fallbackClassName = ConstantCSSClass("--templ-css-class-safe-name")

// Class returns a sanitized CSS class name.
func Class(name string) CSSClass {
	if !safeClassName.MatchString(name) {
		return fallbackClassName
	}
	return SafeClass(name)
}

// SafeClass bypasses CSS class name validation.
func SafeClass(name string) CSSClass {
	return ConstantCSSClass(name)
}

// CSSClass provides a class name.
type CSSClass interface {
	ClassName() string
}

// ConstantCSSClass is a string constant of a CSS class name.
type ConstantCSSClass string

// ClassName of the CSS class.
func (css ConstantCSSClass) ClassName() string {
	return string(css)
}

// ComponentCSSClass is a templ.CSS
type ComponentCSSClass struct {
	// ID of the class, will be autogenerated.
	ID string
	// Definition of the CSS.
	Class SafeCSS
}

// ClassName of the CSS class.
func (css ComponentCSSClass) ClassName() string {
	return css.ID
}

// CSSID calculates an ID.
func CSSID(name string, css string) string {
	sum := sha256.Sum256([]byte(css))
	hp := hex.EncodeToString(sum[:])[0:4]
	return fmt.Sprintf("%s_%s", name, hp)
}

type cssContextKey string

const contextKeyRenderedClasses = cssContextKey("renderedClasses")

// RenderedCSSClassesFromContext returns a set of the CSS classes that have already been
// rendered to the response.
// Deprecated: Use the combined InitializeRenderedItemsContext function which shares a single StringSet for CSS and Scripts.
func RenderedCSSClassesFromContext(ctx context.Context) (context.Context, *StringSet) {
	if classes, ok := ctx.Value(contextKeyRenderedClasses).(*StringSet); ok {
		return ctx, classes
	}
	rc := &StringSet{}
	ctx = context.WithValue(ctx, contextKeyRenderedClasses, rc)
	return ctx, rc
}

// NewCSSMiddleware creates HTTP middleware that renders a global stylesheet of ComponentCSSClass
// CSS if the request path matches, or updates the HTTP context to ensure that any handlers that
// use templ.Components skip rendering <style> elements for classes that are included in the global
// stylesheet. By default, the stylesheet path is /styles/templ.css
func NewCSSMiddleware(next http.Handler, classes ...ComponentCSSClass) CSSMiddleware {
	return CSSMiddleware{
		Path:       "/styles/templ.css",
		CSSHandler: NewCSSHandler(classes...),
		Next:       next,
	}
}

// CSSMiddleware renders a global stylesheet.
type CSSMiddleware struct {
	Path       string
	CSSHandler CSSHandler
	Next       http.Handler
}

func (cssm CSSMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == cssm.Path {
		cssm.CSSHandler.ServeHTTP(w, r)
		return
	}
	// Add registered classes to the context.
	ctx, classes := RenderedCSSClassesFromContext(r.Context())
	for _, c := range cssm.CSSHandler.Classes {
		classes.Add(c.ClassName())
	}
	// Serve the request. Templ components will use the updated context
	// to know to skip rendering <style> elements for any component CSS
	// classes that have been included in the global stylesheet.
	cssm.Next.ServeHTTP(w, r.WithContext(ctx))
}

// NewCSSHandler creates a handler that serves a stylesheet containing the CSS of the
// classes passed in. This is used by the CSSMiddleware to provide global stylesheets
// for templ components.
func NewCSSHandler(classes ...ComponentCSSClass) CSSHandler {
	return CSSHandler{
		Classes: classes,
	}
}

// CSSHandler is a HTTP handler that serves CSS.
type CSSHandler struct {
	Classes []ComponentCSSClass
}

func (cssh CSSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css")
	for _, c := range cssh.Classes {
		w.Write([]byte(c.Class))
	}
}

// RenderCSS renders a <style> element with CSS content, if the styles have not already been rendered.
// Deprecated: Upgrade generation and runtime to use the RenderCSSItems function.
func RenderCSS(ctx context.Context, w io.Writer, classes []CSSClass) (err error) {
	if len(classes) == 0 {
		return nil
	}
	_, rc := RenderedCSSClassesFromContext(ctx)
	sb := new(strings.Builder)
	for _, c := range classes {
		if ccc, ok := c.(ComponentCSSClass); ok {
			if !rc.Contains(ccc.ClassName()) {
				sb.WriteString(string(ccc.Class))
				rc.Add(ccc.ClassName())
			}
		}
	}
	if sb.Len() > 0 {
		if _, err = io.WriteString(w, `<style type="text/css">`); err != nil {
			return err
		}
		if _, err = io.WriteString(w, sb.String()); err != nil {
			return err
		}
		if _, err = io.WriteString(w, `</style>`); err != nil {
			return err
		}
	}
	return nil
}

// RenderCSSItems renders the CSS to the writer, if the items haven't already been rendered.
func RenderCSSItems(ctx context.Context, w io.Writer, classes ...CSSClass) (err error) {
	if len(classes) == 0 {
		return nil
	}
	rc, ok := ctx.Value(contextKeyRenderedItems).(*StringSet)
	if !ok {
		rc = &StringSet{}
	}
	sb := new(strings.Builder)
	for _, c := range classes {
		if ccc, ok := c.(ComponentCSSClass); ok {
			if !rc.ContainsClass(ccc.ClassName()) {
				sb.WriteString(string(ccc.Class))
				rc.AddClass(ccc.ClassName())
			}
		}
	}
	if sb.Len() > 0 {
		if _, err = io.WriteString(w, `<style type="text/css">`); err != nil {
			return err
		}
		if _, err = io.WriteString(w, sb.String()); err != nil {
			return err
		}
		if _, err = io.WriteString(w, `</style>`); err != nil {
			return err
		}
	}
	return nil
}

// SafeCSS is CSS that has been sanitized.
type SafeCSS string

// SanitizeCSS sanitizes CSS properties to ensure that they are safe.
func SanitizeCSS(property, value string) SafeCSS {
	p, v := safehtml.SanitizeCSS(property, value)
	return SafeCSS(p + ":" + v + ";")
}

// General purpose StringSet. Used by the Script and CSS middleware.

// StringSet is a set of strings.
type StringSet struct {
	ss map[string]struct{}
}

// Add string s to the set.
func (rc *StringSet) Add(s string) {
	if rc.ss == nil {
		rc.ss = map[string]struct{}{}
	}
	rc.ss[s] = struct{}{}
}

// Contains returns true if s is within the set.
func (rc *StringSet) Contains(s string) bool {
	if rc.ss == nil {
		return false
	}
	_, ok := rc.ss[s]
	return ok
}

// AddScript with name s to the set.
func (rc *StringSet) AddScript(s string) {
	rc.Add("script_" + s)
}

// ContainsScript returns true if the script with name s is within the set.
func (rc *StringSet) ContainsScript(s string) bool {
	return rc.Contains("script_" + s)
}

// AddClass with name s to the set.
func (rc *StringSet) AddClass(s string) {
	rc.Add("class_" + s)
}

// ContainsClass returns true if the script with name s is within the set.
func (rc *StringSet) ContainsClass(s string) bool {
	return rc.Contains("class_" + s)
}

// All returns a slice of all items in the set.
func (rc *StringSet) All() (values []string) {
	values = make([]string, len(rc.ss))
	var index int
	for k := range rc.ss {
		values[index] = k
		index++
	}
	sort.Strings(values)
	return values
}

// Hyperlink sanitization.

// FailedSanitizationURL is returned if a URL fails sanitization checks.
const FailedSanitizationURL = SafeURL("about:invalid#TemplFailedSanitizationURL")

// URL sanitizes the input string s and returns a SafeURL.
func URL(s string) SafeURL {
	if i := strings.IndexRune(s, ':'); i >= 0 && !strings.ContainsRune(s[:i], '/') {
		protocol := s[:i]
		if !strings.EqualFold(protocol, "http") && !strings.EqualFold(protocol, "https") && !strings.EqualFold(protocol, "mailto") {
			return FailedSanitizationURL
		}
	}
	return SafeURL(s)
}

// SafeURL is a URL that has been sanitized.
type SafeURL string

// Script handling.

// SafeScript encodes unknown parameters for safety.
func SafeScript(functionName string, params ...interface{}) string {
	encodedParams := make([]string, len(params))
	for i := 0; i < len(encodedParams); i++ {
		enc, _ := json.Marshal(params[i])
		encodedParams[i] = EscapeString(string(enc))
	}
	sb := new(strings.Builder)
	sb.WriteString(functionName)
	sb.WriteRune('(')
	sb.WriteString(strings.Join(encodedParams, ","))
	sb.WriteRune(')')
	return sb.String()
}

type renderedItemsContextKey int

const contextKeyRenderedItems = renderedItemsContextKey(0)

// InitializeRenderedItemsContext initializes context used to store internal state used during rendering.
func InitializeRenderedItemsContext(ctx context.Context) context.Context {
	if _, ok := ctx.Value(contextKeyRenderedItems).(*StringSet); ok {
		return ctx
	}
	rs := &StringSet{}
	ctx = context.WithValue(ctx, contextKeyRenderedItems, rs)
	return ctx
}

// ComponentScript is a templ Script template.
type ComponentScript struct {
	// Name of the script, e.g. print.
	Name string
	// Function to render.
	Function string
	// Call of the function in JavaScript syntax, including parameters.
	// e.g. print({ x: 1 })
	Call string
}

type scriptContextKey string

const contextKeyRenderedScripts = scriptContextKey("scripts")

// RenderedScriptsFromContext returns a set of the scripts that have already been
// rendered to the response.
// Deprecated: Use InitializeRenderedItemsContext function which shares a single StringSet for CSS and Scripts.
func RenderedScriptsFromContext(ctx context.Context) (context.Context, *StringSet) {
	if classes, ok := ctx.Value(contextKeyRenderedScripts).(*StringSet); ok {
		return ctx, classes
	}
	rs := &StringSet{}
	ctx = context.WithValue(ctx, contextKeyRenderedScripts, rs)
	return ctx, rs
}

// RenderScripts renders a <script> element, if the script has not already been rendered.
// Deprecated: Upgrade generation and runtime to use the RenderScriptItems function.
func RenderScripts(ctx context.Context, w io.Writer, scripts ...ComponentScript) (err error) {
	if len(scripts) == 0 {
		return nil
	}
	_, rs := RenderedScriptsFromContext(ctx)
	sb := new(strings.Builder)
	for _, s := range scripts {
		if !rs.Contains(s.Name) {
			sb.WriteString(s.Function)
			rs.Add(s.Name)
		}
	}
	if sb.Len() > 0 {
		if _, err = io.WriteString(w, `<script type="text/javascript">`); err != nil {
			return err
		}
		if _, err = io.WriteString(w, sb.String()); err != nil {
			return err
		}
		if _, err = io.WriteString(w, `</script>`); err != nil {
			return err
		}
	}
	return nil
}

// RenderScriptItems renders a <script> element, if the script has not already been rendered.
func RenderScriptItems(ctx context.Context, w io.Writer, scripts ...ComponentScript) (err error) {
	if len(scripts) == 0 {
		return nil
	}
	rs, ok := ctx.Value(contextKeyRenderedItems).(*StringSet)
	if !ok {
		rs = &StringSet{}
	}
	sb := new(strings.Builder)
	for _, s := range scripts {
		if !rs.ContainsScript(s.Name) {
			sb.WriteString(s.Function)
			rs.AddScript(s.Name)
		}
	}
	if sb.Len() > 0 {
		if _, err = io.WriteString(w, `<script type="text/javascript">`); err != nil {
			return err
		}
		if _, err = io.WriteString(w, sb.String()); err != nil {
			return err
		}
		if _, err = io.WriteString(w, `</script>`); err != nil {
			return err
		}
	}
	return nil
}
