// Filter value patterns copied from api/openapi/angelia.v1.yaml; only a value
// that matches is sent, so the API never answers 400 to a half-typed filter.

/** components.parameters.LabelFilter items. */
export const LABEL_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}=.{0,253}$/
/** components.schemas.Kind (the `source` filter). */
export const KIND_PATTERN = /^[a-z0-9][a-z0-9-]{0,62}$/
/** components.schemas.NameOrId (the `watch` filter). */
export const NAME_OR_ID_PATTERN = /^([a-z0-9][a-z0-9._-]{0,62}|[0-7][0-9A-HJKMNP-TV-Z]{25})$/

/** `undefined` when the value is empty or invalid. */
export function validOrUndefined(value: string, pattern: RegExp): string | undefined {
  return value && pattern.test(value) ? value : undefined
}

/** An error message for a non-empty value that does not match. */
export function invalidHelp(value: string, pattern: RegExp, message: string): string | undefined {
  return value && !pattern.test(value) ? message : undefined
}
