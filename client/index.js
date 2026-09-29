function defaultBaseURL() {
  return new URL("../", import.meta.url).pathname
}

function joinURL(baseURL, path) {
  const base = baseURL.endsWith("/") ? baseURL : `${baseURL}/`
  return new URL(String(path || "").replace(/^\/+/, ""), new URL(base, globalThis.location?.href)).toString()
}

function encodePath(path) {
  return String(path || "")
    .split("/")
    .filter(Boolean)
    .map(encodeURIComponent)
    .join("/")
}

function queryString(params = {}) {
  const query = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value == null || value === "" || value === false) continue
    if (Array.isArray(value)) {
      for (const item of value) query.append(key, item)
      continue
    }
    query.set(key, value)
  }
  const text = query.toString()
  return text ? `?${text}` : ""
}

function toFileList(files) {
  if (Array.isArray(files)) return files.filter(Boolean)
  if (typeof FileList !== "undefined" && files instanceof FileList) return Array.from(files)
  return files ? [files] : []
}

function splitTags(input) {
  if (Array.isArray(input)) return input.flatMap(splitTags)
  if (input == null) return []
  return String(input)
    .split(",")
    .map((tag) => tag.trim())
    .filter(Boolean)
}

function uniqueTags(tags = []) {
  return [...new Set(splitTags(tags))]
}

async function parseResponse(response) {
  if (response.status === 204) return undefined
  const contentType = response.headers.get("content-type") || ""
  if (contentType.includes("application/json")) return response.json()
  return response.text()
}

export function createBlobstoreClient({ baseURL = defaultBaseURL(), backend = globalThis.fetch } = {}) {
  async function request(path = "", options = {}) {
    const headers = new Headers(options.headers || {})
    let body = options.body
    if (options.json !== undefined) {
      headers.set("content-type", "application/json")
      body = JSON.stringify(options.json)
    }
    const response = await backend(joinURL(baseURL, path), {
      ...options,
      headers,
      body,
      credentials: options.credentials || "same-origin",
    })
    if (!response.ok) {
      const detail = await parseResponse(response).catch(() => response.statusText)
      const message = typeof detail === "string" ? detail : detail?.error || response.statusText
      throw new Error(message || `Blobstore request failed with ${response.status}`)
    }
    return parseResponse(response)
  }

  return {
    upload(files, options = {}) {
      const pending = toFileList(files)
      if (!pending.length) return Promise.resolve([])
      const form = new FormData()
      for (const file of pending) form.append("files[]", file)
      for (const tag of uniqueTags(options.tags)) form.append("tags", tag)
      return request("", { method: "POST", body: form })
    },
    list(params = {}) {
      return request(`_meta${queryString(params)}`)
    },
    listUploads(params = {}) {
      return this.list(params)
    },
    listWithBlobs(params = {}) {
      return this.list({ ...params, include: "blob" })
    },
    getMeta(hash) {
      return request(`_meta/${encodeURIComponent(hash)}`)
    },
    updateTags(hash, tags = []) {
      return request(`_meta/${encodeURIComponent(hash)}`, {
        method: "PATCH",
        json: { tags: uniqueTags(tags) },
      })
    },
    blobURL(hash, filename = "") {
      const suffix = filename ? `/${encodePath(filename)}` : ""
      return joinURL(baseURL, `${encodeURIComponent(hash)}${suffix}`)
    },
    resizedURL(hash, width, filename) {
      return joinURL(baseURL, `${encodeURIComponent(hash)}/w${encodeURIComponent(width)}/${encodePath(filename)}`)
    },
  }
}

const client = createBlobstoreClient()

export default client
