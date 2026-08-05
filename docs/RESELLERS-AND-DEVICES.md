# Resellers, Shared Quota and Device Limits

Three features that customers and resellers touch directly. Read the scope notes
before selling against any of them — two do less than their names suggest, and
that is stated here rather than discovered in production.

## Shared quota across every protocol

One customer, one quota, however many protocols they hold.

Give every one of a customer's configs the same **Subscription ID**. Their
subscription link then returns all of them, and their traffic is billed against
a single allowance. A 20 GB customer holding WireGuard, OpenVPN, L2TP and seven
others has 20 GB in total, spent wherever they like — not 20 GB each.

This works because quota is keyed on the account's email, and configs sharing a
subscription share that identity. Accounting stays central even when the configs
run on different nodes in different countries, which is what stops one allowance
being spendable once per node.

**Not verified yet:** the same email on two *Xray-native* inbounds (VMess, VLESS,
Trojan) has not been tested against a live Xray. Xray keys its own user
statistics on the email, and whether two inbounds sharing one collide has not
been confirmed on a real server. The non-Xray protocols are unaffected — they
account through nftables and RADIUS, which are keyed on email by design.

## Device limits

**Read this before promising customers an "HWID lock".**

No VPN protocol here carries a hardware identity. WireGuard has a public key,
OpenVPN a certificate, L2TP a username. None of them tell you what machine is at
the other end.

The only moment a device is distinguishable is when a client app fetches the
subscription and identifies itself in the HTTP request. So the limit caps **how
many devices may fetch the config**, not how many may use one once fetched. A
customer who copies a config by hand onto a second phone is not counted.

That is what every panel advertising an HWID limit actually enforces, and it is
genuinely useful: it stops an account being pasted into a group chat, which is
the sharing that actually happens.

### Setting it

**Device Limit** on the client form. 0 means unlimited, matching every other cap
in the panel. It is separate from **IP Limit** on purpose — that one counts
source addresses at connection time and cannot see past a NAT. They answer
different questions and setting both is reasonable.

A customer holding ten protocols has **one** device budget, not ten.

### How a device is recognised

An explicit device header when the client app sends one, otherwise the
User-Agent. The **IP address is deliberately excluded**: phones change address
constantly, cell to wifi and tower to tower, and counting that would exhaust a
customer's limit within a day of ordinary use. The address is recorded beside
each device for you to look at, not used to tell them apart.

Two identical phones running the same app version will count as one device. That
is the honest limit of what an HTTP request reveals, and it errs toward admitting
a real customer rather than refusing one.

### What happens at the limit

A new device gets a 403 explaining what happened and what to do. Devices already
admitted keep working, **including when you lower the limit** — tightening a plan
does not cut off customers mid-month.

The gate fails **open**. If the database errors or the subscription cannot be
resolved, the request is served. This is deliberate: it is not an authorization
check, and turning a panel hiccup into every customer losing their configs at
once is far worse than one extra device.

Remove a device from the account when a customer replaces a phone, and the slot
frees up.

## Separate reseller panel

Resellers can log in at a different URL from the admin panel. Set **Reseller
Base Path** in panel settings; leave it empty and both roles share one path,
which is what an existing install already does.

Logging in on the wrong path redirects you to your own panel.

**What this is for:** blast radius, not secrecy. A reseller's URL is handed to
every person who sells for you and it spreads — chats, saved bookmarks, borrowed
machines. Sharing one path means all of those people also know where the panel
that administers your whole fleet lives, so an attack on it needs no discovery
step. Two paths keep that location out of everyone's normal workflow.

**What this is not:** an access control. A path nobody published is not a
permission. Every route stays reachable by direct request and permissions are
what refuse it — a reseller who guesses the admin path is turned away by the
permission middleware, exactly as before the split existed. Do not treat a secret
URL as a security boundary.

### What each role sees

A reseller sees only the accounts they created. This is enforced at the query
and fails closed: an error resolving ownership returns nothing rather than
falling through to an unfiltered list.

**An admin still sees every account on their inbounds, including a reseller's.**
That is the current behaviour and it is deliberate: you are responsible for the
servers, so you need to see what is running on them, support customers who
escalate, and act on abuse. If you want that hidden too, it is a separate change
with real consequences worth discussing first.

Reseller traffic balances, minimum sale sizes, days-per-GB and per-inbound access
are unchanged and documented in the panel itself.
