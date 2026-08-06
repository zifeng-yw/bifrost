import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Form, FormControl, FormField, FormItem, FormLabel, FormMessage } from "@/components/ui/form";
import { Switch } from "@/components/ui/switch";
import { getErrorMessage, setProviderFormDirtyState, useAppDispatch } from "@/lib/store";
import {
	type CodexOAuthStartResponse,
	providersApi,
	useDisconnectCodexOAuthMutation,
	useGetCodexOAuthConnectionQuery,
	useGetCodexOAuthFlowQuery,
	useStartCodexOAuthMutation,
	useUpdateProviderMutation,
} from "@/lib/store/apis/providersApi";
import type { ModelProvider } from "@/lib/types/config";
import { openaiConfigFormSchema, type OpenAIConfigFormSchema } from "@/lib/types/schemas";
import { RbacOperation, RbacResource, useRbac } from "@enterprise/lib";
import { zodResolver } from "@hookform/resolvers/zod";
import { ExternalLink } from "lucide-react";
import { useEffect, useState } from "react";
import { useForm, type Resolver } from "react-hook-form";
import { toast } from "sonner";
import { buildProviderUpdatePayload } from "../views/utils";

interface OpenAIConfigFormFragmentProps {
	provider: ModelProvider;
}

export function OpenAIConfigFormFragment({ provider }: OpenAIConfigFormFragmentProps) {
	const dispatch = useAppDispatch();
	const hasUpdateProviderAccess = useRbac(RbacResource.ModelProvider, RbacOperation.Update);
	const providerCodexOAuthEnabled = provider.openai_config?.codex_oauth ?? false;
	const [updateProvider, { isLoading: isUpdatingProvider }] = useUpdateProviderMutation();
	const [startCodexOAuth, { isLoading: isStartingCodexOAuth }] = useStartCodexOAuthMutation();
	const [disconnectCodexOAuth, { isLoading: isDisconnectingCodexOAuth }] = useDisconnectCodexOAuthMutation();
	const [oauthFlow, setOAuthFlow] = useState<CodexOAuthStartResponse>();
	const { data: oauthConnection, refetch: refetchOAuthConnection } = useGetCodexOAuthConnectionQuery(provider.name);
	const { data: oauthFlowStatus } = useGetCodexOAuthFlowQuery(
		{ provider: provider.name, flowId: oauthFlow?.flow_id ?? "" },
		{
			skip: !oauthFlow,
			pollingInterval: oauthFlow ? Math.max(oauthFlow.interval * 1000, 1000) : 0,
		},
	);
	const form = useForm<OpenAIConfigFormSchema, any, OpenAIConfigFormSchema>({
		resolver: zodResolver(openaiConfigFormSchema) as Resolver<OpenAIConfigFormSchema, any, OpenAIConfigFormSchema>,
		mode: "onChange",
		reValidateMode: "onChange",
		defaultValues: {
			disable_store: providerCodexOAuthEnabled || (provider.openai_config?.disable_store ?? false),
			codex_oauth: providerCodexOAuthEnabled,
		},
	});
	const codexOAuthEnabled = form.watch("codex_oauth") || oauthConnection?.connected === true;

	useEffect(() => {
		dispatch(setProviderFormDirtyState(form.formState.isDirty));
	}, [form.formState.isDirty, dispatch]);

	useEffect(() => {
		form.reset({
			disable_store: providerCodexOAuthEnabled || (provider.openai_config?.disable_store ?? false),
			codex_oauth: providerCodexOAuthEnabled,
		});
	}, [form, provider.name, provider.openai_config?.disable_store, providerCodexOAuthEnabled]);

	useEffect(() => {
		if (!oauthFlow || !oauthFlowStatus || oauthFlowStatus.status === "pending") return;
		if (oauthFlowStatus.status === "complete") {
			toast.success("ChatGPT connected successfully");
			form.reset({
				disable_store: true,
				codex_oauth: true,
			});
			dispatch(providersApi.util.invalidateTags(["Providers", { type: "ProviderKeys", id: provider.name }]));
			refetchOAuthConnection();
		} else {
			toast.error("Failed to connect ChatGPT", {
				description: oauthFlowStatus.error,
			});
		}
		setOAuthFlow(undefined);
	}, [dispatch, form, oauthFlow, oauthFlowStatus, provider.name, refetchOAuthConnection]);

	const handleConnectCodexOAuth = async () => {
		try {
			const response = await startCodexOAuth(provider.name).unwrap();
			setOAuthFlow(response);
			window.open(response.verification_uri, "_blank", "noopener,noreferrer");
		} catch (err) {
			toast.error("Failed to start ChatGPT connection", {
				description: getErrorMessage(err),
			});
		}
	};

	const handleDisconnectCodexOAuth = async () => {
		try {
			await disconnectCodexOAuth(provider.name).unwrap();
			setOAuthFlow(undefined);
			form.reset({
				disable_store: form.getValues("disable_store"),
				codex_oauth: false,
			});
			toast.success("ChatGPT disconnected");
		} catch (err) {
			toast.error("Failed to disconnect ChatGPT", {
				description: getErrorMessage(err),
			});
		}
	};

	const onSubmit = (data: OpenAIConfigFormSchema) => {
		const normalizedData = {
			...data,
			disable_store: codexOAuthEnabled || data.disable_store,
		};
		updateProvider(
			buildProviderUpdatePayload(provider, {
				openai_config: {
					disable_store: normalizedData.disable_store,
					codex_oauth: normalizedData.codex_oauth,
				},
			}),
		)
			.unwrap()
			.then(() => {
				toast.success("OpenAI configuration updated successfully");
				form.reset(normalizedData);
			})
			.catch((err) => {
				toast.error("Failed to update OpenAI configuration", {
					description: getErrorMessage(err),
				});
			});
	};

	return (
		<Form {...form}>
			<form onSubmit={form.handleSubmit(onSubmit)} className="space-y-6 px-4 md:px-6" data-testid="provider-config-openai-content">
				<div className="space-y-4">
					<FormField
						control={form.control}
						name="disable_store"
						render={({ field }) => (
							<FormItem>
								<div className="flex items-center justify-between space-x-2">
									<div className="space-y-0.5">
										<FormLabel>Disable Store</FormLabel>
										<p className="text-muted-foreground text-xs">
											With the Responses API, store defaults to true, and when it is on, the generated response is stored for later
											retrieval via API. OpenAI exposes endpoints to retrieve and delete stored responses, so your response IDs become
											durable server-side objects instead of one-shot IDs.
											{codexOAuthEnabled && " Codex OAuth requires store=false; disconnect ChatGPT to change this setting."}
										</p>
									</div>
									<FormControl>
										<Switch
											data-testid="provider-openai-disable-store-switch"
											size="md"
											checked={codexOAuthEnabled || field.value}
											disabled={!hasUpdateProviderAccess || codexOAuthEnabled}
											onCheckedChange={(checked) => {
												field.onChange(checked);
												form.trigger("disable_store");
											}}
										/>
									</FormControl>
								</div>
								<FormMessage />
							</FormItem>
						)}
					/>
					<div className="space-y-3 rounded-lg border p-4" data-testid="provider-openai-codex-oauth-card">
						<div className="flex items-start justify-between gap-4">
							<div className="space-y-1">
								<FormLabel>ChatGPT / Codex OAuth</FormLabel>
								<p className="text-muted-foreground text-xs">
									Connect a ChatGPT account for model discovery and streaming Responses. Credentials are exchanged and stored by the Bifrost
									backend; they are never returned to this browser. Requests always use store=false.
								</p>
							</div>
							{oauthConnection?.connected ? (
								<Button
									type="button"
									variant="destructive"
									disabled={!hasUpdateProviderAccess}
									isLoading={isDisconnectingCodexOAuth}
									onClick={handleDisconnectCodexOAuth}
									data-testid="provider-openai-codex-oauth-disconnect"
								>
									Disconnect
								</Button>
							) : (
								<Button
									type="button"
									disabled={!hasUpdateProviderAccess || !!oauthFlow}
									isLoading={isStartingCodexOAuth}
									onClick={handleConnectCodexOAuth}
									data-testid="provider-openai-codex-oauth-connect"
								>
									Connect ChatGPT
								</Button>
							)}
						</div>
						{oauthConnection?.connected && <p className="text-xs text-green-600">Connected</p>}
						{oauthFlow && (
							<div className="bg-muted space-y-2 rounded-md p-3">
								<p className="text-sm font-medium">Enter this one-time code on OpenAI:</p>
								<Input value={oauthFlow.user_code} readOnly showCopyButton inputClassName="font-mono text-base tracking-widest" />
								<Button
									type="button"
									variant="outline"
									size="sm"
									onClick={() => window.open(oauthFlow.verification_uri, "_blank", "noopener,noreferrer")}
								>
									Open OpenAI <ExternalLink className="ml-1 size-3.5" />
								</Button>
								<p className="text-muted-foreground text-xs">Waiting for authorization…</p>
							</div>
						)}
					</div>
				</div>

				<div className="flex justify-end space-x-2 pb-6">
					<Button
						type="submit"
						disabled={!form.formState.isDirty || !form.formState.isValid || !hasUpdateProviderAccess || isUpdatingProvider}
						isLoading={isUpdatingProvider}
					>
						Save OpenAI Configuration
					</Button>
				</div>
			</form>
		</Form>
	);
}