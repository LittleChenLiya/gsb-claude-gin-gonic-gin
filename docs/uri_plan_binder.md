# Explicit URI binder (`gin.UriBinder`)

`gin.UriBinder` is an explicit, precompiled binder for **route (path)
parameters**. While `Context.ShouldBindUri` reflects over the destination
struct on every request and builds an intermediate `map[string][]string`, a
`UriBinder` is compiled once for a fixed struct type and reused:

- the binding plan (field paths, parameter names, aliases, required flags and
  target types) is computed once, at startup;
- per request, the router's already matched `Params` are converted directly —
  the request URL is **never re-parsed**;
- each matched route parameter is converted **at most once** per request;
- the destination is written only when **every** check passes, so a failed
  request can never leave a half-populated struct.

`ShouldBindUri` keeps working exactly as before; the two mechanisms coexist.

## Compiling a plan

Compile once, typically at package initialisation:

```go
type ArticleQuery struct {
    ID   int `uri:"id,required"`
    Page int `uri:"page,required"`
}

var articleBinder = gin.MustNewUriBinder(ArticleQuery{})
```

Use `gin.NewUriBinder` if you prefer handling the compilation error instead of
panicking.

Compilation fails (before any request is served) when:

- two fields bind the **same parameter name** — a primary name colliding with
  another primary name or alias (`duplicate` error kind);
- a field type cannot be populated from a single route value (slices, maps and
  arbitrary structs are not supported; use a scalar field or implement
  `binding.BindUnmarshaler` / `encoding.TextUnmarshaler`).

## Binding in a handler

```go
r.GET("/articles/:id/pages/:page", func(c *gin.Context) {
    var q ArticleQuery
    if err := c.ShouldBindUriWith(articleBinder, &q); err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"msg": err.Error()})
        return
    }
    // q.ID and q.Page are set together; on error q was left untouched.
})
```

You can also call `binder.Bind(&q, c.Params)` or
`binder.BindContext(c, &q)` directly.

## Tag reference

```go
type Params struct {
    // name     — route parameter name (defaults to the Go field name)
    // required — the parameter must be present, else binding fails
    // alias    — one or more alternative accepted names, separated by "|"
    ID    int    `uri:"id,required,alias=article_id|articleId"`
    Page  int    `uri:"page"`                 // optional: keeps prior value if absent
    Label string `uri:"label"`                // plain string
    Small int8   `uri:"small"`                // integer range enforced (e.g. -128..127)
    Skip  int    `uri:"-"`                    // never bound
    When  time.Time `uri:"when,parser=encoding.TextUnmarshaler"`
}
```

- Primary names win over aliases when both are present.
- Aliases are alternative *names* for the same field, not extra values.
- Every accepted name (primary plus aliases) must be unique across the struct;
  collisions are compile errors.
- Supported scalar kinds: `string`, all signed/unsigned integers, `bool`,
  `float32`/`float64` and `time.Duration` (e.g. `1500ms`). Pointer fields and
  anonymous embedded structs (value or pointer) are supported.
- Types implementing `binding.BindUnmarshaler` (method `UnmarshalParam(string)
  error`) or, with `parser=encoding.TextUnmarshaler`, `encoding.TextUnmarshaler`
  use the custom conversion.

## Errors

All failures return `*binding.UriParamError` (use `errors.As`), which always
carries the offending **route parameter name** in `Param`:

| `Kind` constant           | Meaning                                                 |
| ------------------------- | ------------------------------------------------------- |
| `binding.UriErrorMissing` | a `required` parameter was not in `Params`              |
| `binding.UriErrorInvalid` | conversion failed: bad syntax, integer out of range, or the custom converter returned an error |
| `binding.UriErrorConflict`| two fields bind the same name (compile-time only)       |

Integer overflow is reported explicitly (`value ... out of range for ...`) and
remains discoverable via `errors.Is(err, strconv.ErrRange)`. Custom converter
errors are wrapped but remain reachable through `errors.Is` / `errors.As`.

```go
var pe *binding.UriParamError
if errors.As(err, &pe) {
    log.Printf("route param %q failed: %v", pe.Param, pe.Err)
}
```

## Atomicity

Binding converts into a private copy of the destination. The original struct
is overwritten in a single assignment only after all parameters are present
and all conversions succeed. Given `id=12` and `page=bad`, the destination
retains all of its previous values; when both are valid, both fields update
together. Optional parameters that are absent are simply left unchanged.

## Relationship to `ShouldBindUri`

`ShouldBindUri` remains the general-purpose binder (maps, `default=`,
`collection_format`, validator tags, etc.). Prefer the explicit `UriBinder`
when the destination is a fixed scalar struct bound on a hot path and you want
compile-time name-conflict detection, required/range handling without the
validator, stable parameter-named errors and the no-half-struct guarantee.
