import type { Problem } from './types'

/** An API failure: the RFC 9457 problem the server sent, when it sent one. */
export class ApiError extends Error {
  readonly status: number
  readonly problem?: Problem

  constructor(status: number, problem?: Problem) {
    super(problem?.detail ?? problem?.title ?? `request failed (${status})`)
    this.name = 'ApiError'
    this.status = status
    this.problem = problem
  }
}

function asProblem(body: unknown): Problem | undefined {
  if (typeof body === 'object' && body !== null && 'title' in body) return body as Problem
  return undefined
}

interface Result<T> {
  data?: T
  error?: unknown
  response: Response
}

/** Returns the success body of an openapi-fetch call or throws an ApiError. */
export async function unwrap<T>(call: Promise<Result<T>>): Promise<T> {
  const { data, error, response } = await call
  if (data === undefined) throw new ApiError(response.status, asProblem(error))
  return data
}

/** A success body with the response's `ETag`, the version to send back in `If-Match`. */
export interface Versioned<T> {
  data: T
  etag?: string
}

/** As `unwrap`, keeping the `ETag` header of the response. */
export async function unwrapVersioned<T>(call: Promise<Result<T>>): Promise<Versioned<T>> {
  const { data, error, response } = await call
  if (data === undefined) throw new ApiError(response.status, asProblem(error))
  return { data, etag: response.headers.get('ETag') ?? undefined }
}

/** A conditional write refused with `412 precondition-failed`. */
export function isPreconditionFailed(err: unknown): boolean {
  return err instanceof ApiError && err.status === 412
}
