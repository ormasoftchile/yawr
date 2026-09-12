import { createHash } from 'crypto';
import { isScalar, parseDocument, type Scalar } from 'yaml';
import { mapScalar, type ScalarMapping } from './presentationScalar';
import { expressionTextMatches, pointerParts, type ExpressionRegion, type ExpressionResolveReply } from './expressionPresentationProtocol';

export function resolveExpressionScalars(source: string, reply?: ExpressionResolveReply): Array<ScalarMapping & { expression: ExpressionRegion }> {
  if (reply?.status !== 'resolved') return [];
  const doc = parseDocument(source, { keepSourceTokens: true, prettyErrors: false, strict: true, uniqueKeys: true });
  const mapped: Array<ScalarMapping & { expression: ExpressionRegion }> = [];
  for (const expression of reply.regions) {
    try {
      const node = doc.getIn(pointerParts(expression.yaml_path), true);
      if (!isScalar(node) || typeof node.value !== 'string' || !node.range ||
        node.range[0] !== expression.range.start || node.range[1] !== expression.range.end ||
        doc.errors.some(e => e.code === 'DUPLICATE_KEY' || e.pos[0] <= node.range![2])) continue;
      const digest = 'sha256:' + createHash('sha256').update(node.value, 'utf8').digest('hex');
      if (!expressionTextMatches(node.value, expression, digest)) continue;
      mapped.push({ ...mapScalar(node as Scalar<string>), expression });
    } catch { /* Ambiguous/incomplete source is not paintable. */ }
  }
  return mapped;
}
