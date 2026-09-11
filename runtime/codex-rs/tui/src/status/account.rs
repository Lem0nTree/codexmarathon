use codex_protocol::account::PlanType;

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum StatusAccountDisplay {
    ChatGpt {
        email: Option<String>,
        plan: Option<String>,
    },
    ApiKey,
}

/// Formats the account identity shown in the TUI status surfaces.
pub(crate) fn format_account_label(
    account: Option<&StatusAccountDisplay>,
    plan_type: Option<PlanType>,
) -> Option<String> {
    match account {
        Some(StatusAccountDisplay::ChatGpt { email, plan }) => match (email, plan) {
            (Some(email), Some(plan)) => Some(format!("{email}({plan})")),
            (Some(email), None) => Some(format!(
                "{email}({})",
                plan_type.map(super::plan_type_display_name)?
            )),
            _ => None,
        },
        Some(StatusAccountDisplay::ApiKey) => Some("API key".to_string()),
        None => None,
    }
}
