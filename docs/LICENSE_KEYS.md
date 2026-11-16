# Licence files

A licence is a signed file that the operator verifies **offline**. Nothing is sent anywhere, and the product works in an
isolated network. Enforcement is **off by default**: without `--enforce-license` the operator behaves exactly as configured and
only reports the licence. Design and commercial context: [COMMERCIAL.md](COMMERCIAL.md).

Status: implemented and unit-tested (2026-11-16). Not yet run in a cluster. No licence has been issued to anyone and nothing is
sold.

## What a tier allows, under enforcement

| Tier | Mode | What enforcement does |
|---|---|---|
| none, invalid, expired, or over the node limit | Observe-only | Turns observe-only on, does not start the applier, and says why in the log |
| `starter` | Observe-only | The same |
| `professional` | Assisted | The applier may run. Every change needs a human approval, even if the policy says `requireApproval: false` |
| `enterprise` | Automated allowed | Nothing is restricted. The policy still decides |

Enforcement can only make the configuration more conservative. A licence never turns writing on: an operator configured as
observe-only stays observe-only with any licence. It never stops measuring, never rolls back what runs, and never blocks a
rollback.

The licence is read once, at start-up. An operator that keeps running past the expiry date keeps its mode until it restarts;
then it drops to observe-only.

## Vendor side: issue a licence

```bash
rightsize keygen --out vendor
```

```bash
rightsize license issue --key vendor.key --id L-0001 --customer "Acme" --tier professional --max-nodes 40 --days 365 --out acme.license
```

`vendor.key` is the signing key: it never goes to a customer or into a cluster. `vendor.pub` is what the operator trusts.

## Customer side: verify and install

```bash
rightsize license verify --file acme.license --public-key vendor.pub
```

```bash
kubectl -n kubecost-auto-hpa-system create secret generic kubecost-license --from-file=license.json=acme.license --from-file=license.pub=vendor.pub
```

Chart values (no template change is needed):

```yaml
operator:
  extraArgs:
    - --license-file=/license/license.json
    - --license-public-key-file=/license/license.pub
    - --enforce-license
  extraVolumes:
    - {name: license, secret: {secretName: kubecost-license}}
  extraVolumeMounts:
    - {name: license, mountPath: /license, readOnly: true}
```

Check what the operator decided:

```bash
kubectl -n kubecost-auto-hpa-system logs deploy/kubecost-auto-hpa-operator | grep -i licence
```

The metric `kubecost_license_info{tier,enforced,valid}` is always 1; alert on `valid="false"`. The customer name is not a
label.

## What a licence file contains

```json
{"payload": "<base64 of the licence JSON>", "keyID": "<id of the signing key>", "signature": "<base64 Ed25519 signature>"}
```

The payload holds `id`, `customer`, `tier`, `maxNodes` (0 = not limited), `notBefore`, `expires`. The signature covers the exact
payload bytes. Editing any field invalidates it.

## Limits

* The public key is given by flag. Anyone who can change the operator's flags or its Secret can point it at their own key, or
  remove `--enforce-license`. In an Apache-2.0 product this is by design: the licence gates a commercial agreement, it is not
  copy protection. A vendor build that embeds the key is a build-time decision outside this repository.
* The node limit is checked at start-up against the node count at that moment.
* There is no revocation list. A licence is valid until it expires.
* No billing system exists behind it ([COMMERCIAL.md](COMMERCIAL.md)).
