use std::path::PathBuf;
use std::sync::Arc;
use std::sync::Mutex;

use codex_app_server_protocol::MarathonAccount;
use codex_app_server_protocol::MarathonStatusResponse;
use codexmarathon_transfer::ExportReport;
use codexmarathon_transfer::SecretString;
use ratatui::style::Stylize;
use ratatui::text::Line;
use uuid::Uuid;

use super::ChatWidget;
use crate::app_event::AppEvent;
use crate::app_event::MarathonExportSecretStage;
use crate::bottom_pane::MultiSelectItem;
use crate::bottom_pane::MultiSelectPicker;
use crate::bottom_pane::SecretPromptView;
use crate::bottom_pane::custom_prompt_view::CustomPromptView;

type SecretSlot = Arc<Mutex<Option<SecretString>>>;

pub(super) enum PendingMarathonExport {
    Loading {
        flow_id: Uuid,
    },
    Selecting {
        flow_id: Uuid,
        accounts: Vec<MarathonAccount>,
    },
    Output {
        flow_id: Uuid,
        selected_ids: Vec<String>,
    },
    Passphrase {
        flow_id: Uuid,
        selected_ids: Vec<String>,
        output: PathBuf,
        slot: SecretSlot,
    },
    Confirmation {
        flow_id: Uuid,
        selected_ids: Vec<String>,
        output: PathBuf,
        passphrase: SecretString,
        slot: SecretSlot,
    },
    Running {
        flow_id: Uuid,
    },
}

pub(crate) struct MarathonExportReady {
    pub(crate) selected_ids: Vec<String>,
    pub(crate) output: PathBuf,
    pub(crate) passphrase: SecretString,
}

impl ChatWidget {
    pub(super) fn request_marathon_export(&mut self) {
        if self.pending_marathon_export.is_some() {
            self.add_error_message("A Marathon export is already in progress.".to_string());
            return;
        }
        let flow_id = Uuid::new_v4();
        self.pending_marathon_export = Some(PendingMarathonExport::Loading { flow_id });
        self.app_event_tx
            .send(AppEvent::MarathonExportStart { flow_id });
    }

    pub(crate) fn on_marathon_export_accounts_loaded(
        &mut self,
        flow_id: Uuid,
        result: Result<MarathonStatusResponse, String>,
    ) {
        if !matches!(
            self.pending_marathon_export,
            Some(PendingMarathonExport::Loading { flow_id: pending }) if pending == flow_id
        ) {
            return;
        }
        match result {
            Ok(status) if status.accounts.is_empty() => {
                self.pending_marathon_export = None;
                self.add_error_message(
                    "No Marathon accounts are available to export. Import an account first."
                        .to_string(),
                );
            }
            Ok(status) => {
                let accounts = status.accounts;
                self.pending_marathon_export = Some(PendingMarathonExport::Selecting {
                    flow_id,
                    accounts: accounts.clone(),
                });
                self.show_marathon_export_account_picker(flow_id, accounts, None);
            }
            Err(error) => {
                self.pending_marathon_export = None;
                self.add_error_message(format!(
                    "Could not load Marathon accounts for export: {error}"
                ));
            }
        }
    }

    fn show_marathon_export_account_picker(
        &mut self,
        flow_id: Uuid,
        accounts: Vec<MarathonAccount>,
        warning: Option<&str>,
    ) {
        let items = accounts
            .into_iter()
            .map(|account| {
                let name = if account.alias.is_empty() {
                    account.account_id.clone()
                } else {
                    account.alias.clone()
                };
                let active = account.active.then_some("active");
                let health = (!account.credential_health.is_empty())
                    .then_some(account.credential_health.as_str());
                MultiSelectItem {
                    id: account.account_id,
                    name,
                    description: Some(
                        [active, health]
                            .into_iter()
                            .flatten()
                            .collect::<Vec<_>>()
                            .join(" · "),
                    ),
                    enabled: false,
                    orderable: false,
                    section_break_after: false,
                }
            })
            .collect();
        let subtitle = warning
            .unwrap_or("Choose one or more saved accounts. Space toggles a checkbox.")
            .to_string();
        let picker = MultiSelectPicker::builder(
            "Export Marathon accounts".to_string(),
            Some(subtitle),
            self.app_event_tx.clone(),
        )
        .items(items)
        .on_confirm(move |ids, tx| {
            tx.send(AppEvent::MarathonExportAccountsSelected {
                flow_id,
                account_ids: ids.to_vec(),
            });
        })
        .on_cancel(move |tx| tx.send(AppEvent::MarathonExportCancelled { flow_id }))
        .build();
        self.bottom_pane.show_view(Box::new(picker));
    }

    pub(crate) fn on_marathon_export_accounts_selected(
        &mut self,
        flow_id: Uuid,
        account_ids: Vec<String>,
    ) {
        let Some(PendingMarathonExport::Selecting {
            flow_id: pending,
            accounts,
        }) = self.pending_marathon_export.take()
        else {
            return;
        };
        if pending != flow_id {
            self.pending_marathon_export = Some(PendingMarathonExport::Selecting {
                flow_id: pending,
                accounts,
            });
            return;
        }
        if account_ids.is_empty() {
            self.pending_marathon_export = Some(PendingMarathonExport::Selecting {
                flow_id,
                accounts: accounts.clone(),
            });
            self.show_marathon_export_account_picker(
                flow_id,
                accounts,
                Some("Select at least one account before confirming."),
            );
            return;
        }

        self.pending_marathon_export = Some(PendingMarathonExport::Output {
            flow_id,
            selected_ids: account_ids,
        });
        let default_name = format!(
            "codexmarathon-accounts-{}.cmbackup",
            chrono::Local::now().format("%Y-%m-%d")
        );
        let submit_tx = self.app_event_tx.clone();
        let cancel_tx = self.app_event_tx.clone();
        let prompt = CustomPromptView::new(
            "Export destination".to_string(),
            "Path to a new .cmbackup file".to_string(),
            default_name,
            Some("Existing files will not be overwritten.".to_string()),
            Box::new(move |output| {
                submit_tx.send(AppEvent::MarathonExportOutputSubmitted {
                    flow_id,
                    output: PathBuf::from(output),
                });
            }),
        )
        .with_cancel_callback(Box::new(move || {
            cancel_tx.send(AppEvent::MarathonExportCancelled { flow_id });
        }));
        self.bottom_pane.show_text_prompt(prompt);
    }

    pub(crate) fn on_marathon_export_output_submitted(&mut self, flow_id: Uuid, output: PathBuf) {
        let Some(PendingMarathonExport::Output {
            flow_id: pending,
            selected_ids,
        }) = self.pending_marathon_export.take()
        else {
            return;
        };
        if pending != flow_id {
            self.pending_marathon_export = Some(PendingMarathonExport::Output {
                flow_id: pending,
                selected_ids,
            });
            return;
        }
        let output = if output.is_absolute() {
            output
        } else {
            self.config.cwd.join(output).into_path_buf()
        };
        self.show_marathon_export_passphrase(flow_id, selected_ids, output);
    }

    fn show_marathon_export_passphrase(
        &mut self,
        flow_id: Uuid,
        selected_ids: Vec<String>,
        output: PathBuf,
    ) {
        let slot = Arc::new(Mutex::new(None));
        self.pending_marathon_export = Some(PendingMarathonExport::Passphrase {
            flow_id,
            selected_ids,
            output,
            slot: Arc::clone(&slot),
        });
        self.show_marathon_export_secret_prompt(
            flow_id,
            MarathonExportSecretStage::Passphrase,
            "Backup password",
            "Use at least 12 characters. The password is never logged or saved.",
            slot,
        );
    }

    fn show_marathon_export_secret_prompt(
        &mut self,
        flow_id: Uuid,
        stage: MarathonExportSecretStage,
        title: &str,
        context: &str,
        slot: SecretSlot,
    ) {
        let submit_tx = self.app_event_tx.clone();
        let cancel_tx = self.app_event_tx.clone();
        let submit_slot = Arc::clone(&slot);
        let prompt = SecretPromptView::new(
            title.to_string(),
            Some(context.to_string()),
            Box::new(move |secret| match submit_slot.lock() {
                Ok(mut value) => {
                    *value = Some(secret);
                    submit_tx.send(AppEvent::MarathonExportSecretStageReady { flow_id, stage });
                }
                Err(_) => {
                    cancel_tx.send(AppEvent::MarathonExportCancelled { flow_id });
                }
            }),
            Box::new({
                let tx = self.app_event_tx.clone();
                move || tx.send(AppEvent::MarathonExportCancelled { flow_id })
            }),
        );
        self.bottom_pane.show_view(Box::new(prompt));
    }

    pub(crate) fn on_marathon_export_secret_ready(
        &mut self,
        flow_id: Uuid,
        stage: MarathonExportSecretStage,
    ) -> Option<MarathonExportReady> {
        let pending = self.pending_marathon_export.take()?;
        match (stage, pending) {
            (
                MarathonExportSecretStage::Passphrase,
                PendingMarathonExport::Passphrase {
                    flow_id: pending,
                    selected_ids,
                    output,
                    slot,
                },
            ) if pending == flow_id => {
                let passphrase = slot.lock().ok()?.take()?;
                if let Err(error) = codexmarathon_transfer::validate_export_passphrase(&passphrase)
                {
                    self.add_error_message(error.to_string());
                    self.show_marathon_export_passphrase(flow_id, selected_ids, output);
                    return None;
                }
                let confirmation_slot = Arc::new(Mutex::new(None));
                self.pending_marathon_export = Some(PendingMarathonExport::Confirmation {
                    flow_id,
                    selected_ids,
                    output,
                    passphrase,
                    slot: Arc::clone(&confirmation_slot),
                });
                self.show_marathon_export_secret_prompt(
                    flow_id,
                    MarathonExportSecretStage::Confirmation,
                    "Confirm backup password",
                    "Enter the same password again.",
                    confirmation_slot,
                );
                None
            }
            (
                MarathonExportSecretStage::Confirmation,
                PendingMarathonExport::Confirmation {
                    flow_id: pending,
                    selected_ids,
                    output,
                    passphrase,
                    slot,
                },
            ) if pending == flow_id => {
                let confirmation = slot.lock().ok()?.take()?;
                match codexmarathon_transfer::confirm_export_passphrase(passphrase, confirmation) {
                    Ok(passphrase) => {
                        self.pending_marathon_export =
                            Some(PendingMarathonExport::Running { flow_id });
                        self.add_plain_history_lines(vec![Line::from(vec![
                            "Marathon export ".bold(),
                            "encrypting…".cyan(),
                        ])]);
                        Some(MarathonExportReady {
                            selected_ids,
                            output,
                            passphrase,
                        })
                    }
                    Err(error) => {
                        self.add_error_message(error.to_string());
                        self.show_marathon_export_passphrase(flow_id, selected_ids, output);
                        None
                    }
                }
            }
            (_, pending) => {
                self.pending_marathon_export = Some(pending);
                None
            }
        }
    }

    pub(crate) fn on_marathon_export_cancelled(&mut self, flow_id: Uuid) {
        let matches = self
            .pending_marathon_export
            .as_ref()
            .is_some_and(|pending| match pending {
                PendingMarathonExport::Loading { flow_id: id }
                | PendingMarathonExport::Selecting { flow_id: id, .. }
                | PendingMarathonExport::Output { flow_id: id, .. }
                | PendingMarathonExport::Passphrase { flow_id: id, .. }
                | PendingMarathonExport::Confirmation { flow_id: id, .. } => *id == flow_id,
                PendingMarathonExport::Running { .. } => false,
            });
        if matches {
            self.pending_marathon_export = None;
            self.add_plain_history_lines(vec![Line::from(vec![
                "Marathon export ".bold(),
                "cancelled".yellow(),
            ])]);
        }
    }

    pub(crate) fn on_marathon_export_finished(
        &mut self,
        flow_id: Uuid,
        output: PathBuf,
        result: Result<ExportReport, String>,
    ) {
        if !matches!(
            self.pending_marathon_export,
            Some(PendingMarathonExport::Running { flow_id: pending }) if pending == flow_id
        ) {
            return;
        }
        self.pending_marathon_export = None;
        match result {
            Ok(report) => self.add_plain_history_lines(vec![
                Line::from(vec!["✓ Marathon export complete".green().bold()]),
                Line::from(vec![
                    "  File      ".dim(),
                    output.display().to_string().cyan(),
                ]),
                Line::from(vec![
                    "  Accounts  ".dim(),
                    report.accounts.len().to_string().bold(),
                ]),
                Line::from("  Keep the password separately; it cannot be recovered.".dim()),
            ]),
            Err(error) => self.add_error_message(format!("Marathon export failed: {error}")),
        }
    }
}
