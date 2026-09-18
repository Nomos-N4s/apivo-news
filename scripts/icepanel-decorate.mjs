// Turn a loaded IcePanel model into a document.
//
// scripts/icepanel-push.mjs creates the objects and the connections. That is
// the model, and a model is not yet a document: every box looks the same, no
// box links back to the code, and there is nothing to open. This adds the
// three things that make it readable —
//
//   1. DETAIL on every object: a technology icon, a link to the source on
//      GitHub, the external flag on anything we do not build, a lifecycle
//      status, and a maturity tag.
//   2. DIAGRAMS, one per object that has children, laid out rather than
//      dumped in a heap. In IcePanel a diagram is a view over the model, so
//      these add no new facts — they make the existing ones visible.
//   3. FLOWS over those diagrams, one per cashback workflow, stepping along
//      connections that already exist.
//
// Usage:
//
//   export ICEPANEL_API_KEY='<keyId>:<keySecret>'
//   node scripts/icepanel-decorate.mjs --landscape <id>            # dry run
//   node scripts/icepanel-decorate.mjs --landscape <id> --apply    # writes
//
// Re-runnable: everything is keyed on handleId or looked up before it is
// created, so a second run updates rather than duplicates.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const API = 'https://api.icepanel.io/v1'
const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const MODEL_DOC = path.join(ROOT, 'docs/architecture/icepanel-model.md')
const REPO = 'https://github.com/Nomos-N4s/apivo-news/blob/main'

const argv = process.argv.slice(2)
const apply = argv.includes('--apply')
const landscapeId = (() => {
  const i = argv.indexOf('--landscape')
  return i >= 0 ? argv[i + 1] : null
})()
const versionId = (() => {
  const i = argv.indexOf('--version')
  return i >= 0 ? argv[i + 1] : 'latest'
})()
const apiKey = process.env.ICEPANEL_API_KEY ?? ''

// ------------------------------------------------------------------ mapping

// Technology name in the model -> IcePanel catalogue technology. Keyed on the
// first comma-separated token, which is how the model writes the primary
// technology. Resolved ids rather than names: the catalogue's own search is
// fuzzy enough to return "Go Rod" for "Go".
const ICONS = {
  Go: ['Golang', 'v1unR4Fwt0T4JNgEObBM'],
  'Go 1.26.5': ['Golang', 'v1unR4Fwt0T4JNgEObBM'],
  'Go tests': ['Golang', 'v1unR4Fwt0T4JNgEObBM'],
  'Go packages': ['Golang', 'v1unR4Fwt0T4JNgEObBM'],
  'Astro SSR': ['Astro', 'yASAgpknJPIjLemtedND'],
  'Astro endpoint': ['Astro', 'yASAgpknJPIjLemtedND'],
  Astro: ['Astro', 'yASAgpknJPIjLemtedND'],
  TypeScript: ['TypeScript', 'unq9eBvtMu9rTTSQqqUP'],
  'Postgres 17': ['PostgreSQL', 'AtAwOo48GPChCWOkPkkj'],
  'Postgres schema': ['PostgreSQL', 'AtAwOo48GPChCWOkPkkj'],
  'redis:7.2.4-alpine': ['Redis', 'Yj8CPPXztMD4nMBi04y8'],
  'caddy:2.10-alpine': ['Caddy', '9D65lJdHD26VGjKc7SD1'],
  'Docker Compose': ['Docker', '4QZq23Fxy9oyoCTmURM6'],
  'GitHub-hosted runners': ['GitHub Actions', 'QGDt8z4rPDup1KlbW2r1'],
  'OCI registry': ['Container Registry', '3nLiEemnb8b1U2Yb2cnC'],
  DNS: ['Cloudflare', '1QpL9uV6HAToCZ92sjWg'],
  OIDC: ['Supabase', '1aUmrxbw4ZWRPtTxuf3u'],
  'POSIX sh': ['Bash', '63ukUtrL92bdGu50ehGC'],
  'Web browser': ['Browser', 'G2rgDf2KipUKALP1JM3J'],
  'jerryenebeli/blnk 0.15.2': ['Docker', '4QZq23Fxy9oyoCTmURM6'],
}

// Anything we do not build. IcePanel draws these differently, which is the
// whole point: the boundary of what this team controls should be visible
// without reading a single label.
const EXTERNAL = new Set([
  'supabase-auth', 'feed-publishers', 'translation-provider', 'linkwise',
  'awin', 'retailer', 'payout-rail', 'cloudflare', 'github-actions', 'ghcr',
])

// The maturity vocabulary the whole document set uses, as a tag group.
const MATURITY = {
  built: { name: 'Built', color: 'green' },
  partial: { name: 'Partial', color: 'orange' },
  specified: { name: 'Specified', color: 'grey' },
  retired: { name: 'Retired', color: 'red' },
}
const STATUS_OF = { built: 'live', partial: 'live', specified: 'future', retired: 'deprecated' }

// ------------------------------------------------------------------- model

function readModel() {
  const doc = fs.readFileSync(MODEL_DOC, 'utf8')
  const block = doc.match(/```json\n([\s\S]*?)\n```/)
  if (!block) throw new Error('no JSON block in the model document')
  return JSON.parse(block[1])
}

const maturityOf = (o) =>
  (o.tags ?? []).find((t) => Object.prototype.hasOwnProperty.call(MATURITY, t)) ?? null

const iconFor = (o) => {
  const first = (o.technology ?? '').split(',')[0].trim()
  const hit = ICONS[first]
  return hit ? { name: hit[0], catalogTechnologyId: hit[1] } : null
}

// --------------------------------------------------------------- transport

let writes = 0
const base = () => `/landscapes/${landscapeId}/versions/${versionId}`

async function call(method, endpoint, body) {
  if (method !== 'GET') {
    writes++
    if (!apply) return { id: `dry-${writes}`, dryRun: true }
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
  if (!res.ok) throw new Error(`${method} ${endpoint} -> ${res.status}\n${text.slice(0, 500)}`)
  const parsed = text ? JSON.parse(text) : {}
  return (
    parsed.modelObject ?? parsed.domain ?? parsed.diagram ?? parsed.flow ??
    parsed.tag ?? parsed.tagGroup ?? parsed.modelConnection ?? parsed
  )
}

const rows = (payload, ...keys) => {
  const list = keys.reduce((acc, k) => acc ?? payload?.[k], undefined) ?? payload
  return Array.isArray(list) ? list : Object.values(list ?? {})
}

// ---------------------------------------------------------------- layout

/**
 * lay places children on a diagram. Actors go in a column on the left,
 * external systems in a column on the right, and everything we own in a grid
 * between them — which is the shape a reader expects of a C4 diagram and the
 * reason not to just drop boxes at the origin.
 */
function lay(children, modelIdOf) {
  const W = 256, H = 128, GAPX = 120, GAPY = 80
  const actors = children.filter((c) => c.type === 'actor')
  const ext = children.filter((c) => c.type !== 'actor' && EXTERNAL.has(c.id))
  const own = children.filter((c) => c.type !== 'actor' && !EXTERNAL.has(c.id))
  const cols = Math.max(1, Math.min(4, Math.ceil(Math.sqrt(own.length))))
  const objects = {}
  const put = (c, x, y) => {
    objects[c.id] = { modelId: modelIdOf(c.id), type: c.type, shape: 'box', x, y, width: W, height: H }
  }
  actors.forEach((c, i) => put(c, 0, i * (H + GAPY)))
  const ownX = actors.length ? W + GAPX * 2 : 0
  own.forEach((c, i) => put(c, ownX + (i % cols) * (W + GAPX), Math.floor(i / cols) * (H + GAPY)))
  const extX = ownX + cols * (W + GAPX) + GAPX
  ext.forEach((c, i) => put(c, extX, i * (H + GAPY)))
  return objects
}

// -------------------------------------------------------------------- main

async function main() {
  if (!landscapeId) throw new Error('--landscape <id> is required')
  if (!apiKey) throw new Error('ICEPANEL_API_KEY is not set')

  const model = readModel()
  const byId = new Map(model.objects.map((o) => [o.id, o]))
  console.log(`model: ${model.objects.length} objects, ${model.connections.length} connections`)
  if (!apply) console.log('dry run — nothing will be written.\n')

  // Everything already in the landscape, keyed by our own handleId.
  const live = rows(await call('GET', `${base()}/model/objects`), 'modelObjects', 'objects')
  const idOf = new Map(live.filter((o) => o.handleId).map((o) => [o.handleId, o.id]))
  const missing = model.objects.filter((o) => !idOf.has(o.id))
  if (missing.length) {
    throw new Error(
      `${missing.length} model object(s) are not in the landscape (e.g. ${missing[0].id}).\n` +
        'Run scripts/icepanel-push.mjs --apply first; this script decorates, it does not create.'
    )
  }
  console.log(`matched ${idOf.size} live objects by handleId\n`)

  // ---- 1. tags -----------------------------------------------------------
  const groups = rows(await call('GET', `${base()}/tag-groups`), 'tagGroups')
  let group = groups.find((g) => g.name === 'Maturity')
  if (!group) {
    group = await call('POST', `${base()}/tag-groups`, {
      name: 'Maturity',
      icon: 'star',
      index: groups.length + 1,
      handleId: 'maturity',
    })
    console.log('created tag group "Maturity"')
  }
  const liveTags = rows(await call('GET', `${base()}/tags`), 'tags')
  const tagId = new Map()
  let i = 0
  for (const [key, spec] of Object.entries(MATURITY)) {
    i++
    const found = liveTags.find((t) => t.groupId === group.id && t.name === spec.name)
    if (found) { tagId.set(key, found.id); continue }
    const made = await call('POST', `${base()}/tags`, {
      name: spec.name, color: spec.color, groupId: group.id, index: i, handleId: `maturity-${key}`,
    })
    tagId.set(key, made.id)
    console.log(`  tag ${spec.name} (${spec.color})`)
  }

  // ---- 2. object detail --------------------------------------------------
  let decorated = 0
  for (const o of model.objects) {
    const patch = {}
    const icon = iconFor(o)
    if (icon) patch.icon = icon
    if (EXTERNAL.has(o.id)) patch.external = true
    // Maps on a model object take an operator, not a literal replacement: a
    // plain object is accepted with a 200 and silently discarded, which is a
    // worse failure than a 400 because the run looks like it worked.
    if (o.sourcePath && o.sourcePath !== '.') {
      patch.links = {
        $add: { source: { url: `${REPO}/${o.sourcePath}`, customName: o.sourcePath, index: 0 } },
      }
    }
    const m = maturityOf(o)
    if (m) {
      patch.status = STATUS_OF[m]
      if (tagId.get(m)) patch.tagIds = [tagId.get(m)]
    }
    if (!Object.keys(patch).length) continue
    await call('PATCH', `${base()}/model/objects/${idOf.get(o.id)}`, patch)
    decorated++
  }
  console.log(`\ndetail applied to ${decorated} objects (icon, link, external, status, tag)`)

  // ---- 3. diagrams -------------------------------------------------------
  const childrenOf = new Map()
  for (const o of model.objects) {
    if (!o.parentId) continue
    if (!childrenOf.has(o.parentId)) childrenOf.set(o.parentId, [])
    childrenOf.get(o.parentId).push(o)
  }
  const TYPE_OF_DIAGRAM = { system: 'app-diagram', app: 'component-diagram', store: 'component-diagram' }

  const liveDiagrams = rows(await call('GET', `${base()}/diagrams`), 'diagrams')
  const diagramByHandle = new Map(liveDiagrams.filter((d) => d.handleId).map((d) => [d.handleId, d]))

  // The landscape context: every top-level object on one canvas.
  // A diagram's contents are a sub-resource, not a field on the diagram, and
  // its maps take the same operators the model objects' do.
  const connectionIdOf = new Map(
    rows(await call('GET', `${base()}/model/connections`), 'modelConnections', 'connections')
      .filter((c) => c.handleId)
      .map((c) => [c.handleId, c.id])
  )

  /** setContent lays out the given children and draws every model connection
   *  that runs between two of them. A diagram with boxes and no arrows shows
   *  the parts and hides the thing worth reading. */
  const setContent = async (diagramId, kids) => {
    const present = new Set(kids.map((k) => k.id))
    const objects = lay(kids, (id) => idOf.get(id))

    // Roll each endpoint up to the nearest ancestor that is actually on this
    // diagram. Without this a context diagram is seventeen boxes and two
    // arrows, because nearly every connection in the model runs between
    // nested objects — the member clicks out to a Go package, not to "Apivo".
    // The arrow a reader needs at this altitude is the rolled-up one.
    const onDiagram = (id) => {
      let cur = byId.get(id)
      while (cur && !present.has(cur.id)) cur = cur.parentId ? byId.get(cur.parentId) : null
      return cur?.id ?? null
    }

    const connections = {}
    const seen = new Set()
    for (const c of model.connections) {
      const from = onDiagram(c.sourceId)
      const to = onDiagram(c.targetId)
      if (!from || !to || from === to) continue
      const id = connectionIdOf.get(c.id)
      if (!id) continue
      // One arrow per pair; a dozen package-level calls between the same two
      // systems is one relationship at this altitude, not twelve.
      const pair = `${from}->${to}`
      if (seen.has(pair)) continue
      seen.add(pair)
      connections[c.id] = { modelId: id, originId: from, targetId: to }
    }
    await call('PATCH', `${base()}/diagrams/${diagramId}/content`, {
      objects: { $replace: objects },
      connections: { $replace: connections },
    })
    return { objects: Object.keys(objects).length, connections: Object.keys(connections).length }
  }

  const topLevel = model.objects.filter((o) => !o.parentId)
  const contextDiagram = liveDiagrams.find((d) => d.type === 'context-diagram')
  if (contextDiagram) {
    await call('PATCH', `${base()}/diagrams/${contextDiagram.id}`, { name: 'Apivo — system context' })
    const r = apply
      ? await setContent(contextDiagram.id, topLevel)
      : { objects: topLevel.length, connections: '?' }
    console.log(`\ncontext diagram: ${r.objects} objects, ${r.connections} connections`)
  }

  let made = 0
  let n = liveDiagrams.length
  for (const [parentId, kids] of childrenOf) {
    const parent = byId.get(parentId)
    const type = TYPE_OF_DIAGRAM[parent?.type]
    if (!type) continue
    const handle = `diagram-${parentId}`
    let diagram = diagramByHandle.get(handle)
    if (!diagram) {
      diagram = await call('POST', `${base()}/diagrams`, {
        name: `${parent.name} — ${type === 'app-diagram' ? 'containers' : 'components'}`,
        type,
        modelId: idOf.get(parentId),
        index: ++n,
        handleId: handle,
      })
      made++
    }
    if (apply) {
      const r = await setContent(diagram.id, kids)
      console.log(`  ${parent.name}: ${r.objects} objects, ${r.connections} connections`)
    }
  }
  console.log(`diagrams: ${made} created, ${childrenOf.size - made} already present`)

  // ---- 4. flows ----------------------------------------------------------
  // A flow is a sequence walked over a diagram that already exists. Its steps
  // may only travel along connections in the model, which is the property
  // worth having: a flow cannot describe a call the architecture does not
  // have, so it goes stale loudly rather than quietly.
  const flows = model.flows ?? []
  if (flows.length) {
    const diagramsNow = rows(await call('GET', `${base()}/diagrams`), 'diagrams')
    const diagramFor = new Map(diagramsNow.filter((d) => d.handleId).map((d) => [d.handleId, d.id]))
    const liveFlows = rows(await call('GET', `${base()}/flows`), 'flows')
    const flowByHandle = new Map(liveFlows.filter((f) => f.handleId).map((f) => [f.handleId, f.id]))

    let f = 0
    for (const flow of flows) {
      const diagramId = diagramFor.get(`diagram-${flow.diagramOf}`)
      if (!diagramId) {
        console.log(`  skipped "${flow.name}" — no diagram for ${flow.diagramOf}`)
        continue
      }
      const steps = {}
      flow.steps.forEach((s, i) => {
        const id = `s${i + 1}`
        steps[id] = {
          id,
          index: i,
          type: s.type,
          description: s.description,
          originId: s.originId ?? null,
          targetId: s.targetId ?? null,
          viaId: s.viaId ?? null,
          flowId: null,
          parentId: null,
          paths: null,
        }
      })
      const existing = flowByHandle.get(flow.id)
      if (existing) {
        await call('PATCH', `${base()}/flows/${existing.id}`, {
          name: flow.name,
          steps: { $replace: steps },
        })
      } else {
        await call('POST', `${base()}/flows`, {
          name: flow.name,
          diagramId,
          index: ++f,
          handleId: flow.id,
          showConnectionNames: true,
          steps,
        })
      }
      console.log(`  flow "${flow.name}": ${flow.steps.length} steps`)
    }
  }

  console.log(`\n${apply ? 'wrote' : 'would write'} ${writes} changes.`)
  if (!apply) console.log('Re-run with --apply to make them for real.')
}

main().catch((e) => {
  console.error(`\n${e.message}`)
  process.exit(1)
})
