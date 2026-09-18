// Load the architecture model into an IcePanel landscape.
//
// The model is read from the fenced JSON block in
// docs/architecture/icepanel-model.md, which stays the single source of
// truth. This script holds no copy of it: a model that had to be kept in
// step with a second file would be wrong the first time somebody edited
// one of them.
//
// Usage:
//
//   export ICEPANEL_API_KEY='<keyId>:<keySecret>'      # never committed
//
//   node scripts/icepanel-push.mjs --discover --org <organizationId>
//   node scripts/icepanel-push.mjs --landscape <id>                  # dry run
//   node scripts/icepanel-push.mjs --landscape <id> --apply          # writes
//
// A dry run is the default and prints every request it would make. Writing
// to a landscape is the kind of thing that should be asked for out loud.
//
// An API key is created on the organization management screen in IcePanel
// and is passed as `Authorization: ApiKey <keyId>:<keySecret>`. Keep it in
// the environment; this file must never learn how to read one from disk.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const API = 'https://api.icepanel.io/v1'
const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const MODEL_DOC = path.join(ROOT, 'docs/architecture/icepanel-model.md')

// ---------------------------------------------------------------- arguments

const argv = process.argv.slice(2)
const flag = (name) => argv.includes(`--${name}`)
const value = (name, fallback = null) => {
  const i = argv.indexOf(`--${name}`)
  return i >= 0 && argv[i + 1] && !argv[i + 1].startsWith('--') ? argv[i + 1] : fallback
}

const apply = flag('apply')
const discover = flag('discover')
const landscapeId = value('landscape')
const versionId = value('version', 'latest')
const organizationId = value('org')
const apiKey = process.env.ICEPANEL_API_KEY ?? ''

// ------------------------------------------------------------------- model

/** readModel returns the model document's single JSON block, parsed. */
function readModel() {
  const doc = fs.readFileSync(MODEL_DOC, 'utf8')
  const blocks = [...doc.matchAll(/```json\n([\s\S]*?)\n```/g)]
  if (blocks.length !== 1) {
    throw new Error(
      `expected exactly one JSON block in ${path.relative(ROOT, MODEL_DOC)}, found ${blocks.length}`
    )
  }
  const model = JSON.parse(blocks[0][1])
  for (const key of ['domains', 'objects', 'connections']) {
    if (!Array.isArray(model[key])) throw new Error(`model is missing "${key}"`)
  }
  return model
}

/**
 * orderObjects returns the objects sorted so a parent always precedes its
 * children, because IcePanel needs the parent's generated id before it can
 * be named. It throws on a cycle rather than looping.
 */
function orderObjects(objects) {
  const byId = new Map(objects.map((o) => [o.id, o]))
  const ordered = []
  const state = new Map() // id -> 'visiting' | 'done'
  const visit = (o, trail) => {
    const seen = state.get(o.id)
    if (seen === 'done') return
    if (seen === 'visiting') throw new Error(`parent cycle: ${[...trail, o.id].join(' -> ')}`)
    state.set(o.id, 'visiting')
    if (o.parentId) {
      const parent = byId.get(o.parentId)
      if (!parent) throw new Error(`object "${o.id}" names a parent that is not in the model: ${o.parentId}`)
      visit(parent, [...trail, o.id])
    }
    state.set(o.id, 'done')
    ordered.push(o)
  }
  for (const o of objects) visit(o, [])
  return ordered
}

/**
 * validate checks the model against itself before a single request is made.
 * Everything here is cheaper to find now than half way through a load that
 * has already written rows.
 */
function validate(model) {
  const TYPES = new Set(['actor', 'app', 'component', 'group', 'root', 'store', 'system'])
  const DIRECTIONS = new Set(['outgoing', 'bidirectional'])
  const problems = []

  // IcePanel enforces a strict C4 ladder and rejects anything else with a
  // 422 halfway through a load. These pairs were established against the
  // live API, not read from the documentation, which does not state them:
  // a group holds systems and nothing else, an app and a store hold
  // components, and a component holds nothing at all.
  const OWNS = {
    root: new Set(['system', 'actor', 'group']),
    group: new Set(['system']),
    system: new Set(['app', 'store']),
    app: new Set(['component']),
    store: new Set(['component']),
    component: new Set(),
  }

  const domainIds = new Set(model.domains.map((d) => d.id))
  const objectIds = new Set(model.objects.map((o) => o.id))

  if (domainIds.size !== model.domains.length) problems.push('duplicate domain ids')
  if (objectIds.size !== model.objects.length) problems.push('duplicate object ids')

  for (const o of model.objects) {
    if (!o.name) problems.push(`object "${o.id}" has no name`)
    if (!TYPES.has(o.type)) problems.push(`object "${o.id}" has type "${o.type}", which IcePanel does not accept`)
    if (o.domainId && !domainIds.has(o.domainId)) problems.push(`object "${o.id}" names an unknown domain "${o.domainId}"`)
    if (o.parentId && !objectIds.has(o.parentId)) problems.push(`object "${o.id}" names an unknown parent "${o.parentId}"`)
  }
  const typeOf = new Map(model.objects.map((o) => [o.id, o.type]))
  for (const o of model.objects) {
    const parent = o.parentId ? typeOf.get(o.parentId) : 'root'
    if (parent && OWNS[parent] && !OWNS[parent].has(o.type)) {
      problems.push(`object "${o.id}" is a ${o.type} under a ${parent}; IcePanel refuses that`)
    }
  }
  for (const c of model.connections) {
    if (!objectIds.has(c.sourceId)) problems.push(`connection "${c.id}" starts at an unknown object "${c.sourceId}"`)
    if (!objectIds.has(c.targetId)) problems.push(`connection "${c.id}" ends at an unknown object "${c.targetId}"`)
    if (c.direction && !DIRECTIONS.has(c.direction)) problems.push(`connection "${c.id}" has direction "${c.direction}"`)
  }
  return problems
}

/**
 * sourcePathsExist reports model objects whose cited path is no longer in
 * the tree. The citation is the whole reason the model can be checked at
 * all, so a stale one is worth refusing to load rather than shipping into a
 * diagram somebody will trust.
 */
function sourcePathsExist(model) {
  return model.objects
    .filter((o) => o.sourcePath)
    .filter((o) => !fs.existsSync(path.join(ROOT, o.sourcePath.split('#')[0])))
    .map((o) => `${o.id} -> ${o.sourcePath}`)
}

// --------------------------------------------------------------- transport

/** labelsFor carries what IcePanel has no first-class field for. */
function labelsFor(o) {
  const labels = {}
  if (o.technology) labels.technology = o.technology
  if (o.sourcePath) labels.sourcePath = o.sourcePath
  const status = (o.tags ?? []).find((t) => ['built', 'partial', 'specified', 'retired'].includes(t))
  if (status) labels.maturity = status
  return labels
}

/**
 * connectionName is required by the API; the model carries prose instead.
 * The name is what a reader sees on the arrow, so it is cut at a word
 * boundary rather than at a character count — a label reading "beneath the
 * previ" is worse than a shorter one that ends on a word.
 */
function connectionName(c) {
  const sentence = (c.description ?? '').split(/(?<=[.;])\s/)[0].replace(/[.;,]+$/, '').trim()
  if (!sentence) return c.id
  if (sentence.length <= 60) return sentence
  const cut = sentence.slice(0, 58)
  const boundary = cut.lastIndexOf(' ')
  const kept = (boundary > 24 ? cut.slice(0, boundary) : cut).replace(/[.;,\u2014-]+$/, '').trim()
  // The ellipsis is the honest part: the full sentence is on the connection's
  // description, and a label that stops mid-thought should say that it did.
  return `${kept}\u2026`
}

let requests = 0

async function call(method, endpoint, body) {
  requests++
  if (!apply) {
    console.log(`  [dry-run] ${method} ${endpoint}`)
    if (body) console.log(`            ${JSON.stringify(body)}`)
    return { id: `dry-${requests}` }
  }
  const res = await fetch(`${API}${endpoint}`, {
    method,
    headers: {
      Authorization: `ApiKey ${apiKey}`,
      'Content-Type': 'application/json',
      Accept: 'application/json',
    },
    body: body ? JSON.stringify(body) : undefined,
  })
  const text = await res.text()
  if (!res.ok) throw new Error(`${method} ${endpoint} -> ${res.status} ${res.statusText}\n${text.slice(0, 600)}`)
  const parsed = text ? JSON.parse(text) : {}
  // The API nests the created resource under its own name; take whichever
  // shape carries an id rather than guessing one and failing later.
  return parsed.domain ?? parsed.object ?? parsed.modelObject ?? parsed.connection ?? parsed.modelConnection ?? parsed
}

// -------------------------------------------------------------------- main

async function main() {
  const model = readModel()

  const problems = validate(model)
  const stale = sourcePathsExist(model)
  console.log(
    `model: ${model.domains.length} domains, ${model.objects.length} objects, ${model.connections.length} connections`
  )
  if (problems.length) {
    console.error('\nthe model does not describe itself consistently:')
    for (const p of problems) console.error(`  - ${p}`)
    process.exit(1)
  }
  if (stale.length) {
    console.error(`\n${stale.length} object(s) cite a path that is no longer in the tree:`)
    for (const s of stale) console.error(`  - ${s}`)
    console.error('\nFix the model before loading it. A cited path that does not resolve is the')
    console.error('drift this model exists to make visible.')
    process.exit(1)
  }
  console.log('model is internally consistent and every cited path resolves.\n')

  if (!apiKey && (apply || discover)) {
    console.error('ICEPANEL_API_KEY is not set. Create a key on the IcePanel organization')
    console.error('management screen and export it as "<keyId>:<keySecret>".')
    process.exit(1)
  }

  if (discover) {
    if (!organizationId) {
      console.error('--discover needs --org <organizationId>.')
      process.exit(1)
    }
    const res = await fetch(`${API}/organizations/${organizationId}/landscapes`, {
      headers: { Authorization: `ApiKey ${apiKey}`, Accept: 'application/json' },
    })
    const text = await res.text()
    if (!res.ok) {
      console.error(`listing landscapes failed: ${res.status} ${res.statusText}\n${text.slice(0, 600)}`)
      process.exit(1)
    }
    console.log('landscapes:')
    console.log(text)
    console.log('\nRe-run with --landscape <id> to see the plan.')
    return
  }

  if (!landscapeId) {
    console.error('--landscape <id> is required. Use --discover --org <organizationId> to find one.')
    process.exit(1)
  }

  const base = `/landscapes/${landscapeId}/versions/${versionId}`
  if (!apply) console.log(`dry run against ${base} — nothing will be written.\n`)

  // Objects sit under the version's root object when the model gives them
  // no parent of their own. Its id is read rather than assumed.
  // In a dry run the root is not fetched, so say so rather than printing a
  // null that the real run would never send.
  // Everything below is keyed on handleId, which is the model's own id
  // carried into IcePanel. Reading what is already there first is what makes
  // a second run safe: a load that fails on row 30 of 145 has to be finishable
  // without producing 29 duplicates, and the first one did.
  // There is no single root. IcePanel gives every domain a root object of
  // its own, so a top-level object belongs under the root of ITS domain —
  // parenting them all to the first root that turns up puts every system in
  // whichever domain happened to be created first, which is what the first
  // run of this script did before the roots were looked at properly.
  const rootOfDomain = new Map() // IcePanel domain id -> that domain's root object id
  const existingDomains = new Map()
  const existingObjects = new Map()
  const existingConnections = new Map()

  const rows = (payload, ...keys) => {
    const list = keys.reduce((acc, k) => acc ?? payload?.[k], undefined) ?? payload
    return Array.isArray(list) ? list : Object.values(list ?? {})
  }

  /** readRoots refreshes the domain-to-root map; domains created during the
   *  run bring new roots with them, so it is read again after they exist. */
  const readRoots = async () => {
    for (const o of rows(await call('GET', `${base}/model/objects`), 'modelObjects', 'objects')) {
      if (o?.type === 'root' && o?.domainId) rootOfDomain.set(o.domainId, o.id)
    }
  }

  if (apply) {
    const objects = rows(await call('GET', `${base}/model/objects`), 'modelObjects', 'objects')
    for (const o of objects) {
      if (o?.type === 'root' && o?.domainId) rootOfDomain.set(o.domainId, o.id)
      else if (o.handleId) existingObjects.set(o.handleId, o.id)
    }
    for (const d of rows(await call('GET', `${base}/domains`), 'domains')) {
      if (d.handleId) existingDomains.set(d.handleId, d.id)
    }
    for (const c of rows(await call('GET', `${base}/model/connections`), 'modelConnections', 'connections')) {
      if (c.handleId) existingConnections.set(c.handleId, c.id)
    }
    console.log(
      `${rootOfDomain.size} domain root(s) found\n` +
        `already present: ${existingDomains.size} domains, ${existingObjects.size} objects, ` +
        `${existingConnections.size} connections (matched on handleId)\n`
    )
  }

  let created = 0
  let skipped = 0

  const domainId = new Map()
  console.log(`domains (${model.domains.length}):`)
  for (const d of model.domains) {
    if (existingDomains.has(d.id)) {
      domainId.set(d.id, existingDomains.get(d.id))
      skipped++
      continue
    }
    const row = await call('POST', `${base}/domains`, {
      name: d.name,
      handleId: d.id,
      ...(d.description ? { labels: { description: d.description } } : {}),
    })
    domainId.set(d.id, row.id)
    created++
  }
  // Creating a domain creates its root object, so the map is re-read.
  if (apply) await readRoots()

  /** topLevelParent is the root of the object's own domain. */
  const topLevelParent = (o) => {
    if (!apply) return `<root of domain ${o.domainId ?? 'default'}>`
    const root = rootOfDomain.get(domainId.get(o.domainId))
    if (!root) throw new Error(`no root object for domain "${o.domainId}"; cannot place "${o.id}"`)
    return root
  }

  const objectId = new Map()
  const ordered = orderObjects(model.objects)
  console.log(`objects (${ordered.length}, parents first):`)
  for (const o of ordered) {
    if (existingObjects.has(o.id)) {
      objectId.set(o.id, existingObjects.get(o.id))
      skipped++
      continue
    }
    const row = await call('POST', `${base}/model/objects`, {
      name: o.name,
      type: o.type,
      handleId: o.id,
      parentId: o.parentId ? objectId.get(o.parentId) : topLevelParent(o),
      ...(o.domainId ? { domainId: domainId.get(o.domainId) } : {}),
      ...(o.description ? { description: o.description } : {}),
      labels: labelsFor(o),
    })
    objectId.set(o.id, row.id)
    created++
  }

  console.log(`connections (${model.connections.length}):`)
  for (const c of model.connections) {
    if (existingConnections.has(c.id)) {
      skipped++
      continue
    }
    await call('POST', `${base}/model/connections`, {
      name: connectionName(c),
      handleId: c.id,
      originId: objectId.get(c.sourceId),
      targetId: objectId.get(c.targetId),
      direction: c.direction ?? 'outgoing',
      status: 'live',
      ...(c.description ? { description: c.description } : {}),
      ...(c.technology ? { labels: { technology: c.technology } } : {}),
    })
    created++
  }
  if (apply) console.log(`\ncreated ${created}, skipped ${skipped} already present.`)

  const verb = apply ? 'wrote' : 'would write'
  console.log(
    `\n${verb} ${model.domains.length} domains, ${model.objects.length} objects and ` +
      `${model.connections.length} connections — ${requests} requests.`
  )
  if (!apply) console.log('Re-run with --apply to make these requests for real.')
}

main().catch((err) => {
  console.error(`\n${err.message}`)
  process.exit(1)
})
