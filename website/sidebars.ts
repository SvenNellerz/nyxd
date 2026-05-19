import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

// This runs in Node.js - Don't use client-side code here (browser APIs, JSX...)

/**
 * Creating a sidebar enables you to:
 - create an ordered group of docs
 - render a sidebar for each doc of that group
 - provide next/previous navigation

 The sidebars can be generated from the filesystem, or explicitly defined here.

 Create as many sidebars as you want.
 */
const sidebars: SidebarsConfig = {
  docsSidebar: [
    'overview',
    {
      type: 'category',
      label: 'Getting Started',
      items: ['getting-started/install', 'getting-started/usage'],
    },
    {
      type: 'category',
      label: 'Operations',
      items: [
        'operations/networking',
        'operations/native-network',
        'operations/kernel-requirements',
        'operations/qemu-alpine',
      ],
    },
    {
      type: 'category',
      label: 'Reference',
      items: ['reference/api-reference'],
    },
    {
      type: 'category',
      label: 'Project',
      items: ['project/roadmap', 'project/benchmarks'],
    },
  ],
};

export default sidebars;
