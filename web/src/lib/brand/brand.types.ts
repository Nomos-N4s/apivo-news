// Code generated from internal/platform/brand/brand.go. DO NOT EDIT.
//
// The brand schema is defined exactly once, as the Go types in
// internal/platform/brand/brand.go, and this declaration is derived from
// them. Two readers of one brand file cannot disagree about its shape if
// only one of them is allowed to describe it.
//
// The prose explaining what each field means, and why it is required,
// lives beside those Go types. Regenerate with:
//
//   go test ./internal/platform/brand/ -run TypeScriptDeclaration -update

export interface Brand {
  readonly id: string;
  readonly name: string;
  readonly legal: Legal;
  readonly domains: Domains;
  readonly support: Support;
  readonly assets: Assets;
  readonly theme: Theme;
  readonly defaults: Defaults;
  readonly payout: Payout;
  readonly features: Readonly<Record<string, Readonly<Record<string, boolean>>>>;
}

export interface Legal {
  readonly entity: string;
  readonly jurisdiction: string;
  readonly address: string;
  readonly documents: Readonly<Record<string, Document>>;
}

export interface Document {
  readonly id: string;
  readonly version: string;
}

export interface Domains {
  readonly primary: string;
  readonly aliases: readonly string[];
}

export interface Support {
  readonly general: string;
  readonly legal: string;
  readonly privacy: string;
}

export interface Assets {
  readonly logo: string;
  readonly logoDark: string;
  readonly favicon: string;
}

export interface Theme {
  readonly colours: Readonly<Record<string, string>>;
  readonly typography: Typography;
}

export interface Typography {
  readonly heading: string;
  readonly body: string;
  readonly headingWeight: number;
}

export interface Defaults {
  readonly language: string;
  readonly place: string;
  readonly currency: string;
}

export interface Payout {
  readonly descriptor: string;
}

/**
 * How one field is shaped, as data rather than as a type: a primitive by
 * name, another interface, a list, or a string-keyed map.
 */
export type BrandFieldType =
  | 'string'
  | 'number'
  | 'boolean'
  | { readonly struct: string }
  | { readonly list: BrandFieldType }
  | { readonly map: BrandFieldType };

/** One interface's fields, by the name they carry in the brand file. */
export type BrandInterfaceSchema = Readonly<Record<string, BrandFieldType>>;

/** The interface a whole brand file is checked against. */
export const brandRoot = 'Brand';

/** Every interface in the schema, by name. */
export const brandSchema: Readonly<Record<string, BrandInterfaceSchema>> = {
  Brand: {
    id: 'string',
    name: 'string',
    legal: { struct: 'Legal' },
    domains: { struct: 'Domains' },
    support: { struct: 'Support' },
    assets: { struct: 'Assets' },
    theme: { struct: 'Theme' },
    defaults: { struct: 'Defaults' },
    payout: { struct: 'Payout' },
    features: { map: { map: 'boolean' } },
  },
  Legal: {
    entity: 'string',
    jurisdiction: 'string',
    address: 'string',
    documents: { map: { struct: 'Document' } },
  },
  Document: {
    id: 'string',
    version: 'string',
  },
  Domains: {
    primary: 'string',
    aliases: { list: 'string' },
  },
  Support: {
    general: 'string',
    legal: 'string',
    privacy: 'string',
  },
  Assets: {
    logo: 'string',
    logoDark: 'string',
    favicon: 'string',
  },
  Theme: {
    colours: { map: 'string' },
    typography: { struct: 'Typography' },
  },
  Typography: {
    heading: 'string',
    body: 'string',
    headingWeight: 'number',
  },
  Defaults: {
    language: 'string',
    place: 'string',
    currency: 'string',
  },
  Payout: {
    descriptor: 'string',
  },
};
