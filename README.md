# go-xsapi

[![Go Reference](https://pkg.go.dev/badge/github.com/df-mc/go-xsapi/v2.svg)](https://pkg.go.dev/github.com/df-mc/go-xsapi/v2)

>A Go library for communicating with Xbox Live API.

![Azure_Bit_Gopher.png](https://github.com/ashleymcnamara/gophers/blob/2951dcaac888f5489f762c959b1e1c31af48e92d/Azure_Bit_Gopher.png?raw=true)

## Example

This code demonstrates Device Authorization Code Flow to retrieve access token, and interacts with some of the API endpoints available in Xbox Live.

```go
// Notify for Ctrl+C and other interrupt signals so the user can abort
// the device authorization flow or other operations at any time.
signals, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
defer cancel()

// Use the Device Authorization Flow to sign in to a Microsoft Account.
da, err := MinecraftAndroid.DeviceAuth(signals)
if err != nil {
	panic(fmt.Sprintf("error requesting device authorization flow: %s", err))
}

log.Printf("Sign in to your Microsoft Account at %s using the code %s.",
	da.VerificationURI, da.UserCode)

// Make a context for polling the access token while the user completes sign-in.
// In this case, we allow one minute to complete login (you may configure a longer timeout).
pollCtx, cancel := context.WithTimeout(signals, time.Minute)
defer cancel()
token, err := MinecraftAndroid.DeviceAccessToken(pollCtx, da)
if err != nil {
	panic(fmt.Sprintf("error polling access token in device authorization flow: %s", err))
}
// Use TokenSource so we can always use a valid token in fresh state.
msa := MinecraftAndroid.TokenSource(context.Background(), token)

// Make a SISU session using the Microsoft Account token source.
session := MinecraftAndroid.New(msa, nil)

// Log in to Xbox Live services using the SISU session.
client, err := NewClient(session)
if err != nil {
	panic(fmt.Sprintf("error creating API client: %s", err))
}
// Make sure to close the client when it's done.
defer func() {
	if err := client.Close(); err != nil {
		panic(fmt.Sprintf("error closing API client: %s", err))
	}
}()

log.Printf("Logged in as %s", client.UserInfo().GamerTag)

// Use social (peoplehub) endpoint to search a user using the query.
ctx, cancel := context.WithTimeout(signals, time.Second*15)
defer cancel()
users, err := client.Social().Search(ctx, "Lactyy")
if err != nil {
	panic(fmt.Sprintf("error searching for users: %s", err))
}
if len(users) == 0 {
	panic("no users found")
}

// Use the first user present in the result.
user := users[0]
fmt.Println(user.GamerTag)
```

## HTTP requests

The MPSD, Social, Presence, and Notification clients use Resty v2 internally.
They keep the supplied `http.Client` transport, cookie jar, and redirect policy,
including Xbox authentication and request signing. The supplied client is never
modified.

- REST requests use a 30-second timeout when the supplied client has no positive
  timeout. Set `ClientConfig.HTTPClient.Timeout` to choose a different timeout.
  A shorter caller context deadline still takes precedence.
- Response bodies, including error bodies and decompressed data, are limited to
  16 MiB. Resty reads and closes them before the endpoint processes the response.
- Resty automatic retries are disabled. A failed response does not mean a write
  was rejected. MPSD Join retains its explicit, bounded retry for `412` conflicts.
- Endpoints still check their own accepted status codes and decode JSON even when
  Xbox omits the content type. Social errors retain Xbox codes and `Retry-After`.

OAuth, XAL, and RTA WebSockets keep their existing HTTP clients and lifecycles.
These REST defaults do not put a deadline on a whole multi-request operation or
on time spent waiting for a lock. Pass a context with a deadline for operations
that need an overall budget, and keep health checks around long-running workers.

## Contact

[![Discord Banner 2](https://discordapp.com/api/guilds/623638955262345216/widget.png?style=banner2)](https://discord.gg/U4kFWHhTNR)

### Note: We do not under any circumstance support or endorse the usage of go-xsapi with malicious intent.
