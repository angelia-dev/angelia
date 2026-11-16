import { useMutation } from '@tanstack/react-query'
import { previewTemplate } from '@/api/mutations'
import { ProblemSummary } from '@/components/form'
import { Button } from '@/components/ui/button'

/**
 * Renders a template against the built-in sample event with POST /v1/template-previews (admin)
 * and shows the output or the template's error. The output is always shown as text in a
 * `<pre>`, never as HTML: a template can produce markup.
 */
export function TemplatePreview({ template, label = 'template' }: { template: string; label?: string }) {
  const preview = useMutation({ mutationFn: (t: string) => previewTemplate(t) })
  // A result for another text than the one in the field would mislead: it is hidden once the text changes.
  const result = preview.variables === template ? preview.data : undefined
  return (
    <div className="flex flex-col gap-2">
      <div>
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={preview.isPending || template.trim() === ''}
          aria-label={`Preview ${label} with a sample event`}
          onClick={() => preview.mutate(template)}
        >
          {preview.isPending ? 'Rendering…' : 'Preview'}
        </Button>
      </div>
      {preview.isError && preview.variables === template && <ProblemSummary error={preview.error} />}
      {result && (
        <section aria-label={`Preview of ${label}`} role="status" className="flex flex-col gap-1 text-sm">
          {result.error ? (
            <p className="text-destructive">
              The template fails on the sample event ({result.error.kind}
              {result.error.line != null && `, line ${result.error.line}`}
              {result.error.column != null && `, column ${result.error.column}`}): {result.error.reason}
            </p>
          ) : (
            <>
              <p className="text-xs text-muted-foreground">
                Output on the built-in sample event{result.content_type ? ` (${result.content_type})` : ''}; nothing was sent.
              </p>
              <pre
                tabIndex={0}
                aria-label={`Rendered ${label}`}
                className="max-h-64 overflow-auto whitespace-pre-wrap break-all rounded-lg border border-border bg-muted p-3 text-xs focus-visible:outline-2 focus-visible:outline-primary"
              >
                {result.output}
              </pre>
            </>
          )}
        </section>
      )}
    </div>
  )
}
