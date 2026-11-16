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
