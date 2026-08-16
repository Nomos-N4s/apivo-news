export type Json =
  | string
  | number
  | boolean
  | null
  | { [key: string]: Json | undefined }
  | Json[]

export type Database = {
  public: {
    Tables: {
      account: {
        Row: {
          created_at: string
          display_name: string
          email: string
          id: string
          role: string
        }
        Insert: {
          created_at?: string
          display_name: string
          email: string
          id?: string
          role?: string
        }
        Update: {
          created_at?: string
          display_name?: string
          email?: string
          id?: string
          role?: string
        }
        Relationships: []
      }
      article: {
        Row: {
          approved_at: string
          approved_by: string
          attribution_block: string
          id: string
          published_at: string | null
          source_item_id: string | null
          translation_id: string | null
          withdrawal_reason: string | null
          withdrawn_at: string | null
          withdrawn_by: string | null
        }
        Insert: {
          approved_at?: string
          approved_by: string
          attribution_block: string
          id?: string
          published_at?: string | null
          source_item_id?: string | null
          translation_id?: string | null
          withdrawal_reason?: string | null
          withdrawn_at?: string | null
          withdrawn_by?: string | null
        }
        Update: {
          approved_at?: string
          approved_by?: string
          attribution_block?: string
          id?: string
          published_at?: string | null
          source_item_id?: string | null
          translation_id?: string | null
          withdrawal_reason?: string | null
          withdrawn_at?: string | null
          withdrawn_by?: string | null
        }
        Relationships: [
          {
            foreignKeyName: "article_approved_by_fkey"
            columns: ["approved_by"]
            isOneToOne: false
            referencedRelation: "account"
            referencedColumns: ["id"]
          },
          {
            foreignKeyName: "article_approved_by_fkey"
            columns: ["approved_by"]
            isOneToOne: false
            referencedRelation: "article_provenance"
            referencedColumns: ["approver_id"]
          },
          {
            foreignKeyName: "article_source_item_id_fkey"
            columns: ["source_item_id"]
            isOneToOne: false
            referencedRelation: "article_provenance"
            referencedColumns: ["source_item_id"]
          },
          {
            foreignKeyName: "article_source_item_id_fkey"
            columns: ["source_item_id"]
            isOneToOne: false
            referencedRelation: "source_item"
            referencedColumns: ["id"]
          },
          {
            foreignKeyName: "article_translation_id_fkey"
            columns: ["translation_id"]
            isOneToOne: false
            referencedRelation: "article_provenance"
            referencedColumns: ["translation_id"]
          },
          {
            foreignKeyName: "article_translation_id_fkey"
            columns: ["translation_id"]
            isOneToOne: false
            referencedRelation: "translation"
            referencedColumns: ["id"]
          },
          {
            foreignKeyName: "article_withdrawn_by_fkey"
            columns: ["withdrawn_by"]
            isOneToOne: false
            referencedRelation: "account"
            referencedColumns: ["id"]
          },
          {
            foreignKeyName: "article_withdrawn_by_fkey"
            columns: ["withdrawn_by"]
            isOneToOne: false
            referencedRelation: "article_provenance"
            referencedColumns: ["approver_id"]
          },
        ]
      }
      article_place: {
        Row: {
          article_id: string
          place_id: string
        }
        Insert: {
          article_id: string
          place_id: string
        }
        Update: {
          article_id?: string
          place_id?: string
        }
        Relationships: [
          {
            foreignKeyName: "article_place_article_id_fkey"
            columns: ["article_id"]
            isOneToOne: false
            referencedRelation: "article"
            referencedColumns: ["id"]
          },
          {
            foreignKeyName: "article_place_article_id_fkey"
            columns: ["article_id"]
            isOneToOne: false
            referencedRelation: "article_provenance"
            referencedColumns: ["article_id"]
          },
          {
            foreignKeyName: "article_place_place_id_fkey"
            columns: ["place_id"]
            isOneToOne: false
            referencedRelation: "place"
            referencedColumns: ["id"]
          },
        ]
      }
      consent: {
        Row: {
          account_id: string
          granted_at: string
          id: string
          purpose: string
          revoked_at: string | null
        }
        Insert: {
          account_id: string
          granted_at?: string
          id?: string
          purpose: string
          revoked_at?: string | null
        }
        Update: {
          account_id?: string
          granted_at?: string
          id?: string
          purpose?: string
          revoked_at?: string | null
        }
        Relationships: [
          {
            foreignKeyName: "consent_account_id_fkey"
            columns: ["account_id"]
            isOneToOne: false
            referencedRelation: "account"
            referencedColumns: ["id"]
          },
          {
            foreignKeyName: "consent_account_id_fkey"
            columns: ["account_id"]
            isOneToOne: false
            referencedRelation: "article_provenance"
            referencedColumns: ["approver_id"]
          },
        ]
      }
      domain_event: {
        Row: {
          id: string
          occurred_at: string
          payload: Json
          type: string
        }
        Insert: {
          id?: string
          occurred_at?: string
          payload: Json
          type: string
        }
        Update: {
          id?: string
          occurred_at?: string
          payload?: Json
          type?: string
        }
        Relationships: []
      }
      language: {
        Row: {
          code: string
        }
        Insert: {
          code: string
        }
        Update: {
          code?: string
        }
        Relationships: []
      }
      place: {
        Row: {
          country: string
          id: string
          jurisdiction_override: string | null
          name: string
          parent_id: string | null
          slug: string | null
        }
        Insert: {
          country: string
          id?: string
          jurisdiction_override?: string | null
          name: string
          parent_id?: string | null
          slug?: string | null
        }
        Update: {
          country?: string
          id?: string
          jurisdiction_override?: string | null
          name?: string
          parent_id?: string | null
          slug?: string | null
        }
        Relationships: [
          {
            foreignKeyName: "place_parent_id_fkey"
            columns: ["parent_id"]
            isOneToOne: false
            referencedRelation: "place"
            referencedColumns: ["id"]
          },
        ]
      }
      reader_place: {
        Row: {
          account_id: string
          place_id: string
        }
        Insert: {
          account_id: string
          place_id: string
        }
        Update: {
          account_id?: string
          place_id?: string
        }
        Relationships: [
          {
            foreignKeyName: "reader_place_account_id_fkey"
            columns: ["account_id"]
            isOneToOne: false
            referencedRelation: "account"
            referencedColumns: ["id"]
          },
          {
            foreignKeyName: "reader_place_account_id_fkey"
            columns: ["account_id"]
            isOneToOne: false
            referencedRelation: "article_provenance"
            referencedColumns: ["approver_id"]
          },
          {
            foreignKeyName: "reader_place_place_id_fkey"
            columns: ["place_id"]
            isOneToOne: false
            referencedRelation: "place"
            referencedColumns: ["id"]
          },
        ]
      }
      schema_migrations: {
        Row: {
          dirty: boolean
          version: number
        }
        Insert: {
          dirty: boolean
          version: number
        }
        Update: {
          dirty?: boolean
          version?: number
        }
        Relationships: []
      }
      source: {
        Row: {
          active: boolean
          created_at: string
          etag: string
          id: string
          jurisdiction: string
          language_code: string
          last_modified: string
          last_poll_duplicates: number
          last_poll_error: string | null
          last_poll_retrieved: number
          last_polled_at: string | null
          licence_terms: string
          name: string
          permission_evidence: string | null
          url: string
          usage_rule: string
        }
        Insert: {
          active?: boolean
          created_at?: string
          etag?: string
          id?: string
          jurisdiction: string
          language_code: string
          last_modified?: string
          last_poll_duplicates?: number
          last_poll_error?: string | null
          last_poll_retrieved?: number
          last_polled_at?: string | null
          licence_terms: string
          name: string
          permission_evidence?: string | null
          url: string
          usage_rule?: string
        }
        Update: {
          active?: boolean
          created_at?: string
          etag?: string
          id?: string
          jurisdiction?: string
          language_code?: string
          last_modified?: string
          last_poll_duplicates?: number
          last_poll_error?: string | null
          last_poll_retrieved?: number
          last_polled_at?: string | null
          licence_terms?: string
          name?: string
          permission_evidence?: string | null
          url?: string
          usage_rule?: string
        }
        Relationships: [
          {
            foreignKeyName: "source_language_code_fkey"
            columns: ["language_code"]
            isOneToOne: false
            referencedRelation: "language"
            referencedColumns: ["code"]
          },
        ]
      }
      source_item: {
        Row: {
          content_hash: string
          id: string
          licence_snapshot: string
          original_author: string | null
          original_title: string | null
          permission_evidence_snapshot: string | null
          published_at: string | null
          raw_body: string
          retrieved_at: string
          source_id: string
          source_url: string
          usage_rule_snapshot: string
        }
        Insert: {
          content_hash?: string
          id?: string
          licence_snapshot: string
          original_author?: string | null
          original_title?: string | null
          permission_evidence_snapshot?: string | null
          published_at?: string | null
          raw_body: string
          retrieved_at?: string
          source_id: string
          source_url: string
          usage_rule_snapshot: string
        }
        Update: {
          content_hash?: string
          id?: string
          licence_snapshot?: string
          original_author?: string | null
          original_title?: string | null
          permission_evidence_snapshot?: string | null
          published_at?: string | null
          raw_body?: string
          retrieved_at?: string
          source_id?: string
          source_url?: string
          usage_rule_snapshot?: string
        }
        Relationships: [
          {
            foreignKeyName: "source_item_source_id_fkey"
            columns: ["source_id"]
            isOneToOne: false
            referencedRelation: "article_provenance"
            referencedColumns: ["source_id"]
          },
          {
            foreignKeyName: "source_item_source_id_fkey"
            columns: ["source_id"]
            isOneToOne: false
            referencedRelation: "source"
            referencedColumns: ["id"]
          },
        ]
      }
      translation: {
        Row: {
          cost_microusd: number
          extract: string
          generated_at: string
          headline: string
          id: string
          model: string
          prompt_version: string
          source_item_id: string
          target_locale: string
          unmetered_attempts: number
        }
        Insert: {
          cost_microusd: number
          extract: string
          generated_at?: string
          headline: string
          id?: string
          model: string
          prompt_version: string
          source_item_id: string
          target_locale: string
          unmetered_attempts?: number
        }
        Update: {
          cost_microusd?: number
          extract?: string
          generated_at?: string
          headline?: string
          id?: string
          model?: string
          prompt_version?: string
          source_item_id?: string
          target_locale?: string
          unmetered_attempts?: number
        }
        Relationships: [
          {
            foreignKeyName: "translation_source_item_id_fkey"
            columns: ["source_item_id"]
            isOneToOne: false
            referencedRelation: "article_provenance"
            referencedColumns: ["source_item_id"]
          },
          {
            foreignKeyName: "translation_source_item_id_fkey"
            columns: ["source_item_id"]
            isOneToOne: false
            referencedRelation: "source_item"
            referencedColumns: ["id"]
          },
          {
            foreignKeyName: "translation_target_locale_fkey"
            columns: ["target_locale"]
            isOneToOne: false
            referencedRelation: "language"
            referencedColumns: ["code"]
          },
        ]
      }
      translation_spend: {
        Row: {
          halted_at: string | null
          month: string
          spent_microusd: number
          unmetered_attempts: number
        }
        Insert: {
          halted_at?: string | null
          month: string
          spent_microusd?: number
          unmetered_attempts?: number
        }
        Update: {
          halted_at?: string | null
          month?: string
          spent_microusd?: number
          unmetered_attempts?: number
        }
        Relationships: []
      }
    }
    Views: {
      article_provenance: {
        Row: {
          approved_at: string | null
          approver_email: string | null
          approver_id: string | null
          approver_name: string | null
          article_id: string | null
          attribution_block: string | null
          content_hash: string | null
          generated_at: string | null
          jurisdiction: string | null
          licence_snapshot: string | null
          model: string | null
          original_author: string | null
          permission_evidence: string | null
          prompt_version: string | null
          published_at: string | null
          retrieved_at: string | null
          source_feed_url: string | null
          source_id: string | null
          source_item_id: string | null
          source_name: string | null
          source_published_at: string | null
          source_url: string | null
          target_locale: string | null
          translation_id: string | null
          usage_rule: string | null
          withdrawal_reason: string | null
          withdrawn_at: string | null
          withdrawn_by: string | null
        }
        Relationships: [
          {
            foreignKeyName: "article_withdrawn_by_fkey"
            columns: ["withdrawn_by"]
            isOneToOne: false
            referencedRelation: "account"
            referencedColumns: ["id"]
          },
          {
            foreignKeyName: "article_withdrawn_by_fkey"
            columns: ["withdrawn_by"]
            isOneToOne: false
            referencedRelation: "article_provenance"
            referencedColumns: ["approver_id"]
          },
          {
            foreignKeyName: "translation_target_locale_fkey"
            columns: ["target_locale"]
            isOneToOne: false
            referencedRelation: "language"
            referencedColumns: ["code"]
          },
        ]
      }
    }
    Functions: {
      dearmor: { Args: { "": string }; Returns: string }
      gen_random_uuid: { Args: never; Returns: string }
      gen_salt: { Args: { "": string }; Returns: string }
      is_entitled: {
        Args: { p_account_id: string; p_action: string }
        Returns: boolean
      }
      pgp_armor_headers: {
        Args: { "": string }
        Returns: Record<string, unknown>[]
      }
    }
    Enums: {
      [_ in never]: never
    }
    CompositeTypes: {
      [_ in never]: never
    }
  }
}

type DatabaseWithoutInternals = Omit<Database, "__InternalSupabase">

type DefaultSchema = DatabaseWithoutInternals[Extract<keyof Database, "public">]

export type Tables<
  DefaultSchemaTableNameOrOptions extends
    | keyof (DefaultSchema["Tables"] & DefaultSchema["Views"])
    | { schema: keyof DatabaseWithoutInternals },
  TableName extends DefaultSchemaTableNameOrOptions extends {
    schema: keyof DatabaseWithoutInternals
  }
    ? keyof (DatabaseWithoutInternals[DefaultSchemaTableNameOrOptions["schema"]]["Tables"] &
        DatabaseWithoutInternals[DefaultSchemaTableNameOrOptions["schema"]]["Views"])
    : never = never,
> = DefaultSchemaTableNameOrOptions extends {
  schema: keyof DatabaseWithoutInternals
}
  ? (DatabaseWithoutInternals[DefaultSchemaTableNameOrOptions["schema"]]["Tables"] &
      DatabaseWithoutInternals[DefaultSchemaTableNameOrOptions["schema"]]["Views"])[TableName] extends {
      Row: infer R
    }
    ? R
    : never
  : DefaultSchemaTableNameOrOptions extends keyof (DefaultSchema["Tables"] &
        DefaultSchema["Views"])
    ? (DefaultSchema["Tables"] &
        DefaultSchema["Views"])[DefaultSchemaTableNameOrOptions] extends {
        Row: infer R
      }
      ? R
      : never
    : never

export type TablesInsert<
  DefaultSchemaTableNameOrOptions extends
    | keyof DefaultSchema["Tables"]
    | { schema: keyof DatabaseWithoutInternals },
  TableName extends DefaultSchemaTableNameOrOptions extends {
    schema: keyof DatabaseWithoutInternals
  }
    ? keyof DatabaseWithoutInternals[DefaultSchemaTableNameOrOptions["schema"]]["Tables"]
    : never = never,
> = DefaultSchemaTableNameOrOptions extends {
  schema: keyof DatabaseWithoutInternals
}
  ? DatabaseWithoutInternals[DefaultSchemaTableNameOrOptions["schema"]]["Tables"][TableName] extends {
      Insert: infer I
    }
    ? I
    : never
  : DefaultSchemaTableNameOrOptions extends keyof DefaultSchema["Tables"]
    ? DefaultSchema["Tables"][DefaultSchemaTableNameOrOptions] extends {
        Insert: infer I
      }
      ? I
      : never
    : never

export type TablesUpdate<
  DefaultSchemaTableNameOrOptions extends
    | keyof DefaultSchema["Tables"]
    | { schema: keyof DatabaseWithoutInternals },
  TableName extends DefaultSchemaTableNameOrOptions extends {
    schema: keyof DatabaseWithoutInternals
  }
    ? keyof DatabaseWithoutInternals[DefaultSchemaTableNameOrOptions["schema"]]["Tables"]
    : never = never,
> = DefaultSchemaTableNameOrOptions extends {
  schema: keyof DatabaseWithoutInternals
}
  ? DatabaseWithoutInternals[DefaultSchemaTableNameOrOptions["schema"]]["Tables"][TableName] extends {
      Update: infer U
    }
    ? U
    : never
  : DefaultSchemaTableNameOrOptions extends keyof DefaultSchema["Tables"]
    ? DefaultSchema["Tables"][DefaultSchemaTableNameOrOptions] extends {
        Update: infer U
      }
      ? U
      : never
    : never

export type Enums<
  DefaultSchemaEnumNameOrOptions extends
    | keyof DefaultSchema["Enums"]
    | { schema: keyof DatabaseWithoutInternals },
  EnumName extends DefaultSchemaEnumNameOrOptions extends {
    schema: keyof DatabaseWithoutInternals
  }
    ? keyof DatabaseWithoutInternals[DefaultSchemaEnumNameOrOptions["schema"]]["Enums"]
    : never = never,
> = DefaultSchemaEnumNameOrOptions extends {
  schema: keyof DatabaseWithoutInternals
}
  ? DatabaseWithoutInternals[DefaultSchemaEnumNameOrOptions["schema"]]["Enums"][EnumName]
  : DefaultSchemaEnumNameOrOptions extends keyof DefaultSchema["Enums"]
    ? DefaultSchema["Enums"][DefaultSchemaEnumNameOrOptions]
    : never

export type CompositeTypes<
  PublicCompositeTypeNameOrOptions extends
    | keyof DefaultSchema["CompositeTypes"]
    | { schema: keyof DatabaseWithoutInternals },
  CompositeTypeName extends PublicCompositeTypeNameOrOptions extends {
    schema: keyof DatabaseWithoutInternals
  }
    ? keyof DatabaseWithoutInternals[PublicCompositeTypeNameOrOptions["schema"]]["CompositeTypes"]
    : never = never,
> = PublicCompositeTypeNameOrOptions extends {
  schema: keyof DatabaseWithoutInternals
}
  ? DatabaseWithoutInternals[PublicCompositeTypeNameOrOptions["schema"]]["CompositeTypes"][CompositeTypeName]
  : PublicCompositeTypeNameOrOptions extends keyof DefaultSchema["CompositeTypes"]
    ? DefaultSchema["CompositeTypes"][PublicCompositeTypeNameOrOptions]
    : never

export const Constants = {
  public: {
    Enums: {},
  },
} as const

