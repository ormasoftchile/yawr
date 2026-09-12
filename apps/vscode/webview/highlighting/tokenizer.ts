import { createHighlighterCore } from 'shiki/core';
import { createJavaScriptRegexEngine } from 'shiki/engine/javascript';
import sql from 'shiki/langs/sql.mjs';
import kusto from 'shiki/langs/kusto.mjs';
import powershell from 'shiki/langs/powershell.mjs';
import light from 'shiki/themes/github-light.mjs';
import dark from 'shiki/themes/github-dark.mjs';
import type { ThemeRegistration } from 'shiki';
import { MAX_CODE_UNITS, supportedDescriptor, type Descriptor } from '../../src/presentationProtocol';
import type { ColoredSpan } from '../../src/presentationScalar';

export type Palette = 'light' | 'dark' | 'hc' | 'hc-light';
const contrast = (isLight: boolean): ThemeRegistration => ({
  name: isLight ? 'yawr-hc-light' : 'yawr-hc', type: isLight ? 'light' : 'dark',
  colors: { 'editor.background': isLight ? '#FFFFFF' : '#000000', 'editor.foreground': isLight ? '#000000' : '#FFFFFF' },
  tokenColors: [
    { scope: ['comment'], settings: { foreground: isLight ? '#006400' : '#00FF00' } },
    { scope: ['keyword', 'storage'], settings: { foreground: isLight ? '#000080' : '#FFFF00' } },
    { scope: ['string'], settings: { foreground: isLight ? '#800000' : '#00FFFF' } },
    { scope: ['variable'], settings: { foreground: isLight ? '#800080' : '#FFAAFF' } },
    { scope: ['constant.numeric'], settings: { foreground: isLight ? '#000000' : '#FFFFFF' } },
  ],
});
const themes: Record<Palette, string> = { light: 'github-light', dark: 'github-dark', hc: 'yawr-hc', 'hc-light': 'yawr-hc-light' };
let highlighter: ReturnType<typeof createHighlighterCore> | undefined;
export function warmup() {
  return highlighter ??= createHighlighterCore({ langs: [sql, kusto, powershell],
    themes: [light, dark, contrast(false), contrast(true)], engine: createJavaScriptRegexEngine() });
}
export async function tokenize(text: string, descriptor: Descriptor, palette: Palette): Promise<ColoredSpan[]> {
  if (text.length > MAX_CODE_UNITS) throw new Error('limit-exceeded');
  if (!supportedDescriptor(descriptor)) throw new Error('unsupported-language');
  const engine = await warmup();
  const lang = descriptor.language === 'kql' ? 'kusto' : descriptor.language;
  const tokens = engine.codeToTokensBase(text, { lang, theme: themes[palette] ?? themes.dark });
  const output: ColoredSpan[] = [];
  for (const token of tokens.flat()) {
    if (!token.content.length) continue;
    const end = token.offset + token.content.length;
    output.push({ start: token.offset, end, color: token.color ?? '#808080' });
  }
  return output;
}
