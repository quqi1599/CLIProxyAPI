#!/usr/bin/env node

import { readFileSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';

const args = new Map();
for (let index = 2; index < process.argv.length; index += 2) {
  args.set(process.argv[index], process.argv[index + 1]);
}

const source = args.get('--source');
const output = args.get('--output') || 'content-audit-policy.yaml';
const sourceRevision = args.get('--source-revision') || 'unknown';
if (!source) {
  throw new Error('usage: generate_content_audit_policy.mjs --source <0037_moderation_categories.sql> [--output <path>] [--source-revision <sha>]');
}

const sql = readFileSync(resolve(source), 'utf8');
const categoryPattern = /^\s*\('([^']+)',\s*'((?:''|[^'])*)',\s*'((?:''|[^'])*)',\s*'(critical|high|medium|low)',\s*1704067200000\)[,;]?$/gm;
const keywordPattern = /^\s*\('((?:''|[^'])*)',\s*'txt',\s*'(\[[^']*\])'::jsonb,\s*1704067200000\)[,;]?$/gm;
const unescapeSql = (value) => value.replaceAll("''", "'");

const categories = [];
for (const match of sql.matchAll(categoryPattern)) {
  categories.push({
    code: match[1],
    label: unescapeSql(match[2]),
    description: unescapeSql(match[3]),
    severity: match[4],
  });
}

const keywords = [];
for (const match of sql.matchAll(keywordPattern)) {
  keywords.push({
    text: unescapeSql(match[1]),
    categories: JSON.parse(match[2]),
  });
}

if (categories.length !== 11 || keywords.length !== 642) {
  throw new Error(`unexpected seed size: categories=${categories.length}, keywords=${keywords.length}`);
}

const severityRank = { critical: 4, high: 3, medium: 2, low: 1 };
const categoryByCode = new Map(categories.map((category) => [category.code, category]));
const primaryCategory = (codes) => codes.reduce((best, code) => {
  const candidate = categoryByCode.get(code);
  if (!candidate) throw new Error(`unknown category ${code}`);
  if (!best || severityRank[candidate.severity] > severityRank[best.severity]) return candidate;
  return best;
}, null);

const keywordGroups = new Map(categories.map((category) => [category.code, []]));
for (const keyword of keywords) {
  keywordGroups.get(primaryCategory(keyword.categories).code).push(keyword.text);
}

const globalAllowlist = [
  '接口交互',
  '光口交换机',
];

const ruleAllowlist = {
  cyber: [
    '授权渗透测试',
    '合法渗透测试',
    '防御性恶意软件分析',
    '恶意软件分析报告',
    '勒索软件防御分析',
    '钓鱼网站检测',
    '钓鱼页面检测',
    '密码爆破防护',
    '暴力破解防护',
    '漏洞利用检测',
    '漏洞利用修复',
    '协议逆向分析',
    'authorized penetration testing',
    'defensive malware analysis',
    'ransomware detection',
    'phishing detection',
    'password cracking defense',
    'exploit mitigation',
  ],
  piracy: [
    '兼容性逆向工程',
    '软件安全审计',
    'reverse engineering for interoperability',
  ],
};

const quote = (value) => JSON.stringify(value);
const lines = [
  '# Generated from acking-you/llm-access moderation seed.',
  `# Source revision: ${sourceRevision}`,
  '# Multi-category phrases use the highest-severity category; source order breaks ties.',
  '# The 642 source rows compile to 641 unique runtime patterns after normalization.',
  '# CPA keeps request-level enforcement only; this policy does not ban sessions or API keys.',
  'version: "llm-access-seed-2026-08-19-v1"',
  'global-allowlist:',
  ...globalAllowlist.map((value) => `  - ${quote(value)}`),
  'rules:',
];

for (const category of categories) {
  const categoryKeywords = keywordGroups.get(category.code);
  lines.push(
    `  - id: ${quote(`seed-${category.code}`)}`,
    `    category: ${quote(category.code)}`,
    `    severity: ${quote(category.severity)}`,
    '    keywords:',
    ...categoryKeywords.map((value) => `      - ${quote(value)}`),
  );
  const allowlist = ruleAllowlist[category.code] || [];
  if (allowlist.length > 0) {
    lines.push('    allowlist:', ...allowlist.map((value) => `      - ${quote(value)}`));
  }
}

writeFileSync(resolve(output), `${lines.join('\n')}\n`, { mode: 0o644 });
console.log(`wrote ${output}: ${keywords.length} keywords across ${categories.length} categories`);
